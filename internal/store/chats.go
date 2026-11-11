package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Chat kinds.
const (
	KindChat  = "chat"  // a conversation with the user
	KindTask  = "task"  // one unattended run of a task
	KindAgent = "agent" // a sub-agent spawned by another chat
)

// ChatMeta describes one conversation. The transcript itself is an
// append-only JSONL file next to it.
type ChatMeta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	Model     string    `json:"model"`
	Effort    string    `json:"effort"`
	Parent    string    `json:"parent,omitempty"` // spawning chat, for sub-agents
	Task      string    `json:"task,omitempty"`   // task id, for task runs
	Depth     int       `json:"depth,omitempty"`  // sub-agent nesting level
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	Pinned    bool      `json:"pinned,omitempty"`
	Unread    bool      `json:"unread,omitempty"`
	Context   int       `json:"context"`        // tokens in the live context after the last request
	Window    int       `json:"window"`         // context window of the model
	TokensIn  int64     `json:"tokensIn"`       // cumulative, including cache reads
	TokensOut int64     `json:"tokensOut"`      // cumulative
	CacheRead int64     `json:"cacheRead"`      // cumulative
	Cost      float64   `json:"cost,omitempty"` // cumulative, when the provider reports it
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,80}$`)

// ValidID reports whether id is safe to use as a file or directory name.
func ValidID(id string) bool { return idPattern.MatchString(id) }

func (h *Home) chatPath(id, ext string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("invalid chat id %q", id)
	}
	return h.Path("chats", id+ext), nil
}

// NewChat creates a chat and returns its completed metadata.
func (h *Home) NewChat(meta ChatMeta) (ChatMeta, error) {
	now := time.Now()
	meta.ID = now.Format("20060102-150405") + "-" + RandomID(3)
	meta.Created, meta.Updated = now, now
	if meta.Kind == "" {
		meta.Kind = KindChat
	}
	return meta, h.SaveChat(meta)
}

// SaveChat writes chat metadata.
func (h *Home) SaveChat(meta ChatMeta) error {
	p, err := h.chatPath(meta.ID, ".json")
	if err != nil {
		return err
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return writeAtomic(p, b, 0o600)
}

// Chat loads the metadata of one chat.
func (h *Home) Chat(id string) (ChatMeta, error) {
	var meta ChatMeta
	p, err := h.chatPath(id, ".json")
	if err != nil {
		return meta, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return meta, err
	}
	return meta, json.Unmarshal(b, &meta)
}

// Chats lists all chats, most recently updated first.
func (h *Home) Chats() ([]ChatMeta, error) {
	entries, err := os.ReadDir(h.Path("chats"))
	if err != nil {
		return nil, err
	}
	var out []ChatMeta
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			if meta, err := h.Chat(id); err == nil {
				out = append(out, meta)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// DeleteChat removes a chat, its transcript and its uploads.
func (h *Home) DeleteChat(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid chat id %q", id)
	}
	os.Remove(h.Path("chats", id+".jsonl"))
	os.RemoveAll(h.Path("uploads", id))
	return os.Remove(h.Path("chats", id+".json"))
}

// Append adds one record to a chat's transcript.
func (h *Home) Append(id string, rec any) error {
	p, err := h.chatPath(id, ".jsonl")
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // writes one line, newline included
	enc.SetEscapeHTML(false)     // keep <, > and & readable for grep and other tools
	if err := enc.Encode(rec); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(buf.Bytes())
	return err
}

// Records returns every record of a chat's transcript in order.
func (h *Home) Records(id string) ([]json.RawMessage, error) {
	p, err := h.chatPath(id, ".jsonl")
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []json.RawMessage
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 && json.Valid(line) {
			out = append(out, json.RawMessage(line))
		}
		if err != nil {
			return out, nil
		}
	}
}
