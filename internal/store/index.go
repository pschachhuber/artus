package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Entry is one indexed file: a topic or a skill.
type Entry struct {
	Name        string `json:"name"`
	Path        string `json:"path"` // relative to the home
	Description string `json:"description"`
}

// Task is a standing job the agent runs unattended.
type Task struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"` // active | paused | done
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	Description string `json:"description"` // TASK.md body
	Notes       string `json:"notes"`       // NOTES.md, the agent's working state
}

// ExternalTool is a tool the agent built: a manifest next to a script.
type ExternalTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Command     []string        `json:"command"` // argv, run in the tool's directory
	Secrets     []string        `json:"secrets,omitempty"`
	Timeout     int             `json:"timeout,omitempty"` // seconds
	Dir         string          `json:"-"`
}

// Frontmatter splits a leading "---" block of "key: value" lines from text.
func Frontmatter(text string) (map[string]string, string) {
	fields := map[string]string{}
	text = strings.TrimPrefix(text, "\xef\xbb\xbf")
	if !strings.HasPrefix(text, "---") {
		return fields, text
	}
	head, body, ok := strings.Cut(text[3:], "\n---")
	if !ok {
		return fields, text
	}
	for _, line := range strings.Split(head, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(k, " ") {
			fields[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return fields, strings.TrimLeft(strings.TrimPrefix(body, "\n"), "\n")
}

// WithFrontmatter renders fields in a stable order followed by body.
func WithFrontmatter(fields map[string]string, order []string, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	for _, k := range order {
		if v := fields[k]; v != "" {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	b.WriteString("---\n\n" + strings.TrimSpace(body) + "\n")
	return b.String()
}

// firstLine returns a one-line summary of markdown text.
func firstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line != "" {
			if r := []rune(line); len(r) > 140 {
				return string(r[:140]) + "…"
			}
			return line
		}
	}
	return ""
}

// Topics lists the knowledge-base notes.
func (h *Home) Topics() []Entry {
	var out []Entry
	filepath.WalkDir(h.Path("topics"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		fields, body := Frontmatter(string(b))
		desc := fields["description"]
		if desc == "" {
			desc = firstLine(body)
		}
		rel, _ := h.Rel(p)
		out = append(out, Entry{Name: strings.TrimSuffix(strings.TrimPrefix(rel, "topics/"), ".md"), Path: rel, Description: desc})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Skills lists the installed skills (folders holding a SKILL.md).
func (h *Home) Skills() []Entry {
	var out []Entry
	dirs, _ := os.ReadDir(h.Path("skills"))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		b, err := os.ReadFile(h.Path("skills", d.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		fields, body := Frontmatter(string(b))
		if fields["disabled"] == "true" {
			continue
		}
		name, desc := fields["name"], fields["description"]
		if name == "" {
			name = d.Name()
		}
		if desc == "" {
			desc = firstLine(body)
		}
		out = append(out, Entry{Name: name, Path: "skills/" + d.Name() + "/SKILL.md", Description: desc})
	}
	return out
}

// Tasks lists all tasks.
func (h *Home) Tasks() []Task {
	var out []Task
	dirs, _ := os.ReadDir(h.Path("tasks"))
	for _, d := range dirs {
		if t, err := h.Task(d.Name()); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// Task loads one task by id (its directory name).
func (h *Home) Task(id string) (Task, error) {
	t := Task{ID: id}
	if !ValidID(id) {
		return t, os.ErrNotExist
	}
	b, err := os.ReadFile(h.Path("tasks", id, "TASK.md"))
	if err != nil {
		return t, err
	}
	fields, body := Frontmatter(string(b))
	t.Name, t.Status, t.Model, t.Effort = fields["name"], fields["status"], fields["model"], fields["effort"]
	t.Description = strings.TrimSpace(body)
	if t.Name == "" {
		t.Name = id
	}
	if t.Status == "" {
		t.Status = "active"
	}
	notes, _ := os.ReadFile(h.Path("tasks", id, "NOTES.md"))
	t.Notes = string(notes)
	return t, nil
}

// SaveTask writes a task's TASK.md, keeping NOTES.md untouched.
func (h *Home) SaveTask(t Task) error {
	if !ValidID(t.ID) {
		return os.ErrInvalid
	}
	fields := map[string]string{"name": t.Name, "status": t.Status, "model": t.Model, "effort": t.Effort}
	text := WithFrontmatter(fields, []string{"name", "status", "model", "effort"}, t.Description)
	return writeAtomic(h.Path("tasks", t.ID, "TASK.md"), []byte(text), 0o600)
}

// ExternalTools lists the tools the agent has built under tools/.
func (h *Home) ExternalTools() []ExternalTool {
	var out []ExternalTool
	dirs, _ := os.ReadDir(h.Path("tools"))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		b, err := os.ReadFile(h.Path("tools", d.Name(), "tool.json"))
		if err != nil {
			continue
		}
		var t ExternalTool
		if json.Unmarshal(b, &t) != nil || CheckTool(t) != nil {
			continue
		}
		t.Dir = h.Path("tools", d.Name())
		out = append(out, t)
	}
	return out
}

var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// CheckTool says what is wrong with a tool manifest, if anything. A tool a
// provider rejects would make every new conversation fail, so such a tool is
// not offered at all.
func CheckTool(t ExternalTool) error {
	switch {
	case !toolName.MatchString(t.Name):
		return errors.New("name must be 1-64 letters, digits, _ or -")
	case strings.TrimSpace(t.Description) == "":
		return errors.New("description is missing")
	case len(t.Command) == 0:
		return errors.New("command is missing (an argv list, run in the tool's folder)")
	}
	if len(t.Parameters) > 0 {
		var schema struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(t.Parameters, &schema) != nil || schema.Type != "object" {
			return errors.New(`parameters must be a JSON Schema object: {"type":"object","properties":{…}}`)
		}
	}
	return nil
}
