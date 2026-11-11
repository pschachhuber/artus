package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/sched"
	"github.com/pschachhuber/artus/internal/store"
)

type fsEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// fsList lists a folder of the home, or, without a path, the indexed
// topics and skills.
func (s *Server) fsList(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		reply(w, map[string]any{"topics": s.Home.Topics(), "skills": s.Home.Skills()})
		return
	}
	dir, ok := s.userPath(p, true)
	if !ok {
		refuse(w, http.StatusForbidden, errors.New("not available"))
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		refuse(w, http.StatusNotFound, errors.New("no such folder"))
		return
	}
	out := []fsEntry{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rel, _ := s.Home.Rel(filepath.Join(dir, e.Name()))
		out = append(out, fsEntry{Name: e.Name(), Path: rel, Dir: e.IsDir(), Size: info.Size(), Modified: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return out[i].Name < out[j].Name
	})
	reply(w, map[string]any{"entries": out})
}

func (s *Server) fsRead(w http.ResponseWriter, r *http.Request) {
	path, ok := s.userPath(r.URL.Query().Get("path"), true)
	if !ok {
		refuse(w, http.StatusForbidden, errors.New("not available"))
		return
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		reply(w, map[string]any{"content": "", "exists": false})
		return
	}
	if err != nil || len(b) > 4<<20 {
		bad(w, "cannot open this file as text")
		return
	}
	reply(w, map[string]any{"content": string(b), "exists": true})
}

// fsWrite saves a file the user edited in the interface.
func (s *Server) fsWrite(w http.ResponseWriter, r *http.Request) {
	path, ok := s.userPath(r.URL.Query().Get("path"), false)
	if !ok {
		refuse(w, http.StatusForbidden, errors.New("not editable here"))
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		bad(w, "file too large")
		return
	}
	rel, _ := s.Home.Rel(path)
	if s.Home.Locked(path) {
		err = s.Home.WriteLocked(rel, data)
	} else if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		err = os.WriteFile(path, data, 0o600)
	}
	if err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	s.changed("Edited by you: " + rel)
	reply(w, map[string]bool{"ok": true})
}

func (s *Server) fsDelete(w http.ResponseWriter, r *http.Request) {
	path, ok := s.userPath(r.URL.Query().Get("path"), false)
	rel, _ := s.Home.Rel(path)
	if !ok || !strings.Contains(rel, "/") { // top-level files and folders stay
		refuse(w, http.StatusForbidden, errors.New("cannot delete this"))
		return
	}
	if err := os.RemoveAll(path); err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	s.changed("Deleted by you: " + rel)
	reply(w, map[string]bool{"ok": true})
}

// changed records a snapshot after the user edited something and tells the
// interface to refresh its lists.
func (s *Server) changed(message string) {
	s.Home.Snapshot(message)
	s.Publish("changes", nil)
}

type taskView struct {
	store.Task
	Triggers []triggerView    `json:"triggers"`
	Runs     []store.ChatMeta `json:"runs"`
	Running  bool             `json:"running"`
	Unread   int              `json:"unread"`
}

type triggerView struct {
	sched.Trigger
	Text string `json:"text"`
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	chats, _ := s.Home.Chats()
	running := map[string]bool{}
	for _, id := range s.Engine.Running() {
		running[id] = true
	}
	out := []taskView{}
	for _, t := range s.Home.Tasks() {
		v := taskView{Task: t, Triggers: []triggerView{}, Runs: []store.ChatMeta{}}
		for _, tr := range s.Sched.List("task:" + t.ID) {
			v.Triggers = append(v.Triggers, triggerView{tr, sched.Describe(tr.When)})
		}
		for _, c := range chats {
			if c.Kind != store.KindTask || c.Task != t.ID {
				continue
			}
			if c.Unread {
				v.Unread++
			}
			v.Running = v.Running || running[c.ID]
			if len(v.Runs) < 30 {
				v.Runs = append(v.Runs, c)
			}
		}
		out = append(out, v)
	}
	wakeups := []triggerView{}
	for _, tr := range s.Sched.List("") {
		wakeups = append(wakeups, triggerView{tr, sched.Describe(tr.When)})
	}
	reply(w, map[string]any{"tasks": out, "triggers": wakeups})
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// createTask stores a task from a name and description, then starts a first
// run in which the agent plans the work and sets its own triggers.
func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Description, Model, Effort string }
	if !body(w, r, &in) {
		return
	}
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	if in.Description == "" {
		bad(w, "describe what the task should do")
		return
	}
	if in.Name == "" {
		in.Name = strings.Join(strings.Fields(in.Description), " ")
		if r := []rune(in.Name); len(r) > 50 {
			in.Name = string(r[:50])
		}
	}
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(in.Name), "-"), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" {
		slug = "task"
	}
	id := slug
	for n := 2; ; n++ {
		if _, err := os.Stat(s.Home.Path("tasks", id)); err != nil {
			break
		}
		id = slug + "-" + string(rune('0'+n%10))
		if n > 9 {
			id = slug + "-" + store.RandomID(2)
		}
	}
	task := store.Task{ID: id, Name: in.Name, Status: "active", Model: in.Model, Effort: in.Effort, Description: in.Description}
	if err := s.Home.SaveTask(task); err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	s.changed("New task: " + in.Name)
	chat, err := s.Engine.RunTask(id, "The user just created this task. Work out how to do it, set the triggers it needs with the schedule tool, and do a first run now if that makes sense.")
	if err != nil {
		bad(w, "The task was saved but could not start: %v", err)
		return
	}
	reply(w, map[string]any{"task": task, "chat": chat})
}

func (s *Server) patchTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.Home.Task(r.PathValue("id"))
	if err != nil {
		refuse(w, http.StatusNotFound, errors.New("no such task"))
		return
	}
	var in struct{ Name, Description, Status, Model, Effort *string }
	if !body(w, r, &in) {
		return
	}
	for dst, src := range map[*string]*string{&task.Name: in.Name, &task.Description: in.Description, &task.Status: in.Status, &task.Model: in.Model, &task.Effort: in.Effort} {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	if task.Status != "active" && task.Status != "paused" && task.Status != "done" {
		bad(w, "status must be active, paused or done")
		return
	}
	if err := s.Home.SaveTask(task); err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	s.changed("Task edited by you: " + task.Name)
	s.Publish("tasks", nil)
	reply(w, task)
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Home.Task(id); err != nil {
		refuse(w, http.StatusNotFound, errors.New("no such task"))
		return
	}
	s.Sched.RemoveTarget("task:" + id)
	if chats, err := s.Home.Chats(); err == nil {
		for _, c := range chats {
			if c.Kind == store.KindTask && c.Task == id {
				s.Engine.Stop(c.ID)
				s.Home.DeleteChat(c.ID)
			}
		}
	}
	os.RemoveAll(s.Home.Path("tasks", id))
	s.changed("Task deleted by you: " + id)
	s.Publish("tasks", nil)
	reply(w, map[string]bool{"ok": true})
}

func (s *Server) runTask(w http.ResponseWriter, r *http.Request) {
	chat, err := s.Engine.RunTask(r.PathValue("id"), "Started by the user.")
	if err != nil {
		bad(w, "%v", err)
		return
	}
	reply(w, map[string]string{"chat": chat})
}

func (s *Server) deleteTrigger(w http.ResponseWriter, r *http.Request) {
	s.Sched.Remove(r.PathValue("id"))
	s.Publish("schedule", nil)
	reply(w, map[string]bool{"ok": true})
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	in := s.Home.Config()
	if !body(w, r, &in) {
		return
	}
	if !contains(llm.Efforts, in.Effort) {
		bad(w, "effort must be one of %s", strings.Join(llm.Efforts, ", "))
		return
	}
	if in.Autonomy != "auto" && in.Autonomy != "confirm" {
		bad(w, "autonomy must be auto or confirm")
		return
	}
	for _, m := range append([]string{in.Model}, in.AllowedModels...) {
		if m == "" {
			continue
		}
		if _, _, err := s.LLM.Resolve(m); err != nil {
			bad(w, "%v", err)
			return
		}
	}
	if in.Context.CompactAt <= 0.05 || in.Context.CompactAt > 0.95 {
		bad(w, "context.compactAt must be between 0.05 and 0.95")
		return
	}
	in.Limits.Subagents = clamp(in.Limits.Subagents, 1, 64)
	in.Limits.Depth = clamp(in.Limits.Depth, 0, 4)
	in.Limits.Turns = clamp(in.Limits.Turns, 1, 10000)
	in.Context.ToolOutputChars = clamp(in.Context.ToolOutputChars, 2000, 2_000_000)
	err := s.Home.UpdateConfig(func(c *store.Config) {
		in.Server.Token = c.Server.Token // never set through the API
		*c = in
	})
	if err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	s.Publish("config", nil)
	s.getState(w, r)
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// listModels returns the live model list of every provider that has a key.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") != ""
	models, errs := []llm.Model{}, map[string]string{}
	for _, name := range s.LLM.Names() {
		if name != "mock" && !s.Secrets.Has(strings.ToUpper(name)+"_API_KEY") {
			continue
		}
		list, err := s.LLM.Models(r.Context(), name, refresh)
		if err != nil {
			errs[name] = err.Error()
		}
		models = append(models, list...)
	}
	reply(w, map[string]any{"models": models, "errors": errs})
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	cfg := s.Home.Config().Secrets
	reply(w, map[string]any{"secrets": s.Secrets.Names(), "backend": cfg.Backend, "vault": cfg.Vault})
}

func (s *Server) putSecret(w http.ResponseWriter, r *http.Request) {
	var in struct{ Value string }
	if !body(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Value) == "" {
		bad(w, "the value is empty")
		return
	}
	if err := s.Secrets.Set(r.PathValue("name"), strings.TrimSpace(in.Value)); err != nil {
		bad(w, "%v", err)
		return
	}
	s.Publish("secrets", nil)
	reply(w, map[string]bool{"ok": true})
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	if err := s.Secrets.Set(r.PathValue("name"), ""); err != nil {
		bad(w, "%v", err)
		return
	}
	s.Publish("secrets", nil)
	reply(w, map[string]bool{"ok": true})
}

func (s *Server) listChanges(w http.ResponseWriter, r *http.Request) {
	changes := s.Home.Changes(100)
	if changes == nil {
		changes = []store.Change{}
	}
	reply(w, map[string]any{"changes": changes})
}

func (s *Server) getChange(w http.ResponseWriter, r *http.Request) {
	diff, err := s.Home.Diff(r.PathValue("hash"))
	if err != nil {
		refuse(w, http.StatusNotFound, err)
		return
	}
	reply(w, map[string]string{"diff": diff})
}

func (s *Server) revertChange(w http.ResponseWriter, r *http.Request) {
	if err := s.Home.Revert(r.PathValue("hash")); err != nil {
		refuse(w, http.StatusConflict, err)
		return
	}
	s.Publish("changes", nil)
	s.Publish("tasks", nil)
	reply(w, map[string]bool{"ok": true})
}
