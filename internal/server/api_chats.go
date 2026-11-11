package server

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pschachhuber/artus/internal/agent"
	"github.com/pschachhuber/artus/internal/store"
)

func (s *Server) listChats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind == "" {
		kind = store.KindChat
	}
	all, err := s.Home.Chats()
	if err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	out := []store.ChatMeta{}
	for _, c := range all {
		if c.Kind != kind || (q.Get("task") != "" && c.Task != q.Get("task")) || (q.Get("parent") != "" && c.Parent != q.Get("parent")) {
			continue
		}
		out = append(out, c)
	}
	reply(w, map[string]any{"chats": out, "running": s.Engine.Running()})
}

func (s *Server) createChat(w http.ResponseWriter, r *http.Request) {
	var in struct{ Model, Effort string }
	if !body(w, r, &in) {
		return
	}
	meta, err := s.Engine.NewChat(store.ChatMeta{Model: in.Model, Effort: in.Effort})
	if err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, meta)
}

// slim drops what the interface never shows: the full prompt and tool
// schemas of each segment, and provider-native block payloads.
func slim(recs []agent.Record) []agent.Record {
	for i := range recs {
		if recs[i].T == "system" {
			recs[i].System, recs[i].Tools = "", nil
		}
		if m := recs[i].Msg; m != nil {
			for j := range m.Blocks {
				m.Blocks[j].Raw = nil
			}
		}
	}
	return recs
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	meta, err := s.Home.Chat(id)
	if err != nil {
		refuse(w, http.StatusNotFound, errors.New("no such chat"))
		return
	}
	seq := s.hub.current() // taken first: events after it are applied on top by the client
	if meta.Unread {
		meta.Unread = false
		s.Home.SaveChat(meta)
		s.hub.publish(agent.Event{Chat: id, Type: "chat", Data: meta})
	}
	children := []store.ChatMeta{}
	if all, err := s.Home.Chats(); err == nil {
		for _, c := range all {
			if c.Parent == id {
				children = append(children, c)
			}
		}
	}
	reply(w, map[string]any{
		"meta": meta, "records": slim(s.Engine.Records(id)), "live": s.Engine.Live(id),
		"children": children, "seq": seq,
	})
}

func (s *Server) patchChat(w http.ResponseWriter, r *http.Request) {
	meta, err := s.Home.Chat(r.PathValue("id"))
	if err != nil {
		refuse(w, http.StatusNotFound, errors.New("no such chat"))
		return
	}
	var in struct {
		Title, Model, Effort *string
		Pinned, Unread       *bool
	}
	if !body(w, r, &in) {
		return
	}
	if in.Title != nil && strings.TrimSpace(*in.Title) != "" {
		meta.Title = strings.TrimSpace(*in.Title)
	}
	if in.Model != nil {
		if _, _, err := s.LLM.Resolve(*in.Model); err != nil {
			bad(w, "%v", err)
			return
		}
		if meta.Model != *in.Model {
			meta.Model, meta.Window = *in.Model, 0
		}
	}
	if in.Effort != nil {
		meta.Effort = *in.Effort
	}
	if in.Pinned != nil {
		meta.Pinned = *in.Pinned
	}
	if in.Unread != nil {
		meta.Unread = *in.Unread
	}
	if err := s.Home.SaveChat(meta); err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	s.hub.publish(agent.Event{Chat: meta.ID, Type: "chat", Data: meta})
	reply(w, meta)
}

func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.Engine.Stop(id)
	if all, err := s.Home.Chats(); err == nil {
		for _, c := range all {
			if c.Parent == id {
				s.Engine.Stop(c.ID)
				s.Home.DeleteChat(c.ID)
			}
		}
	}
	if err := s.Home.DeleteChat(id); err != nil {
		refuse(w, http.StatusNotFound, errors.New("no such chat"))
		return
	}
	s.Sched.RemoveTarget("chat:" + id)
	s.hub.publish(agent.Event{Chat: id, Type: "chat_deleted"})
	reply(w, map[string]bool{"ok": true})
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in agent.Input
	if !body(w, r, &in) {
		return
	}
	root := s.Home.Path("uploads", id) + string(filepath.Separator)
	for i, f := range in.Files {
		st, err := os.Stat(f.Path)
		if err != nil || !strings.HasPrefix(filepath.Clean(f.Path), root) {
			bad(w, "attachment %q was not uploaded to this chat", f.Name)
			return
		}
		in.Files[i].Size = st.Size()
	}
	if err := s.Engine.Send(id, in); err != nil {
		bad(w, "%v", err)
		return
	}
	reply(w, map[string]bool{"ok": true})
}

func (s *Server) stopChat(w http.ResponseWriter, r *http.Request) {
	s.Engine.Stop(r.PathValue("id"))
	reply(w, map[string]bool{"ok": true})
}

func (s *Server) compactChat(w http.ResponseWriter, r *http.Request) {
	if err := s.Engine.Compact(r.PathValue("id")); err != nil {
		bad(w, "%v", err)
		return
	}
	reply(w, map[string]bool{"ok": true})
}

// forkChat copies the first `records` records of a chat into a new one, so
// the user can take the conversation a different way from that point.
func (s *Server) forkChat(w http.ResponseWriter, r *http.Request) {
	src, err := s.Home.Chat(r.PathValue("id"))
	if err != nil {
		refuse(w, http.StatusNotFound, errors.New("no such chat"))
		return
	}
	var in struct{ Records int }
	if !body(w, r, &in) {
		return
	}
	raw, err := s.Home.Records(src.ID)
	if err != nil || in.Records < 0 || in.Records > len(raw) {
		bad(w, "cannot fork at record %d", in.Records)
		return
	}
	meta, err := s.Engine.NewChat(store.ChatMeta{Title: src.Title + " (fork)", Model: src.Model, Effort: src.Effort, Window: src.Window})
	if err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	for _, rec := range raw[:in.Records] {
		if err := s.Home.Append(meta.ID, rec); err != nil {
			refuse(w, http.StatusInternalServerError, err)
			return
		}
	}
	reply(w, meta)
}

func (s *Server) chatContext(w http.ResponseWriter, r *http.Request) {
	info, err := s.Engine.Context(r.PathValue("id"))
	if err != nil {
		refuse(w, http.StatusNotFound, errors.New("no such chat"))
		return
	}
	for i := range info.Messages {
		for j := range info.Messages[i].Blocks {
			info.Messages[i].Blocks[j].Raw, info.Messages[i].Blocks[j].UI = nil, nil
		}
	}
	reply(w, info)
}

const maxUpload = 100 << 20

// upload stores the request body as an attachment of a chat.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Home.Chat(id); err != nil {
		refuse(w, http.StatusNotFound, errors.New("no such chat"))
		return
	}
	name := safeName(r.URL.Query().Get("name"))
	dir := s.Home.Path("uploads", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	path := filepath.Join(dir, name)
	for n := 2; ; n++ {
		if _, err := os.Stat(path); err != nil {
			break
		}
		ext := filepath.Ext(name)
		path = filepath.Join(dir, strings.TrimSuffix(name, ext)+"-"+strconv.Itoa(n)+ext)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		refuse(w, http.StatusInternalServerError, err)
		return
	}
	size, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxUpload))
	f.Close()
	if err != nil {
		os.Remove(path)
		refuse(w, http.StatusRequestEntityTooLarge, fmt.Errorf("upload failed (the limit is %d MB)", maxUpload>>20))
		return
	}
	reply(w, agent.Attachment{Path: path, Name: filepath.Base(path), Type: mediaType(path), Size: size})
}

// safeName reduces an uploaded file name to something harmless on every
// file system.
func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127:
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('_')
		}
	}
	name = strings.TrimLeft(b.String(), ".")
	if name == "" {
		name = "file"
	}
	if len(name) > 120 {
		name = name[len(name)-120:]
	}
	return name
}

func mediaType(path string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); t != "" {
		t, _, _ = mime.ParseMediaType(t)
		return t
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	t, _, _ := mime.ParseMediaType(http.DetectContentType(head[:n]))
	return t
}

func (s *Server) answerAsk(w http.ResponseWriter, r *http.Request) {
	var in struct{ Answer string }
	if !body(w, r, &in) {
		return
	}
	if !s.Engine.Answer(r.PathValue("id"), in.Answer) {
		refuse(w, http.StatusGone, errors.New("this question is no longer open"))
		return
	}
	reply(w, map[string]bool{"ok": true})
}

// serveFile shows a file from the home (an attachment, a screenshot, a
// report the agent wrote). Whatever it contains is served inert: sandboxed,
// with no script execution, so agent- or web-authored HTML cannot act in the
// interface's origin.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	path, ok := s.userPath(r.URL.Query().Get("path"), true)
	if !ok {
		refuse(w, http.StatusForbidden, errors.New("not available"))
		return
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		refuse(w, http.StatusNotFound, errors.New("no such file"))
		return
	}
	t := mediaType(path)
	inline := strings.HasPrefix(t, "image/") && t != "image/svg+xml" || t == "application/pdf" || strings.HasPrefix(t, "audio/") || strings.HasPrefix(t, "video/")
	switch {
	case r.URL.Query().Get("download") != "" || (!inline && !strings.HasPrefix(t, "text/")):
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}))
	case !inline:
		t = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", t)
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
	w.Header().Set("Cache-Control", "private, max-age=60")
	http.ServeFile(w, r, path)
}

// userPath resolves a path given by the interface to a location inside the
// home that the user may see through it. Secrets, configuration and browser
// profiles are never served; with readOnly the transcripts and uploads are
// reachable too.
func (s *Server) userPath(p string, readOnly bool) (string, bool) {
	if p == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = s.Home.Path(filepath.FromSlash(p))
	}
	p = filepath.Clean(p)
	rel, ok := s.Home.Rel(p)
	if !ok || rel == "." {
		return "", false
	}
	top, _, _ := strings.Cut(rel, "/")
	switch top {
	case store.SecretsFile, store.ConfigFile, store.ScheduleFile, ".git", ".gitignore", "pass-session":
		return "", false
	case "chats", "uploads", "tmp":
		return p, readOnly
	case "browser":
		if strings.HasPrefix(rel, "browser/profiles") || strings.HasPrefix(rel, "browser/chrome") {
			return "", false
		}
		return p, readOnly || rel == "browser/config.json"
	}
	return p, true
}
