package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
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

// chatGuard is what this process last wrote for a chat: its metadata, and
// the length and running hash of its append-only transcript. The chats
// folder is the harness's alone; a shell command that edits it (to plant a
// system prompt in a transcript, or to make a chat pass for a task run) is
// undone.
type chatGuard struct {
	meta []byte
	size int64     // transcript bytes written so far; -1 until known
	mod  time.Time // the transcript's modification time after the last write
	sum  hash.Hash // of those bytes
}

func (h *Home) chatGuardFor(id string) *chatGuard {
	g := h.chats[id]
	if g == nil {
		g = &chatGuard{size: -1}
		h.chats[id] = g
	}
	return g
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
	h.chatMu.Lock()
	defer h.chatMu.Unlock()
	if err := writeAtomic(p, b, 0o600); err != nil {
		return err
	}
	h.chatGuardFor(meta.ID).meta = b
	return nil
}

// Chat loads the metadata of one chat.
func (h *Home) Chat(id string) (ChatMeta, error) {
	var meta ChatMeta
	p, err := h.chatPath(id, ".json")
	if err != nil {
		return meta, err
	}
	h.chatMu.Lock()
	var b []byte
	if g := h.chats[id]; g != nil && g.meta != nil {
		b = g.meta // what the harness wrote, whatever is on disk now
	}
	h.chatMu.Unlock()
	if b == nil {
		if b, err = os.ReadFile(p); err != nil {
			return meta, err
		}
	}
	return meta, json.Unmarshal(b, &meta)
}

// checkTranscript makes sure a transcript is what this process wrote. Lines
// added behind its back are cut off; a transcript changed in the middle
// cannot be repaired and is reported. Callers hold chatMu.
func (h *Home) checkTranscript(id string, g *chatGuard) (restored bool, err error) {
	p := h.Path("chats", id+".jsonl")
	st, statErr := os.Stat(p)
	if g.size < 0 || (statErr == nil && st.Size() == g.size && st.ModTime().Equal(g.mod)) {
		return false, nil
	}
	data, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if int64(len(data)) >= g.size && bytes.Equal(sha(data[:g.size]), g.sum.Sum(nil)) {
		if int64(len(data)) == g.size {
			g.mod = st.ModTime()
			return false, nil
		}
		if err := os.WriteFile(p, data[:g.size], 0o600); err != nil {
			return false, err
		}
		if st, err := os.Stat(p); err == nil {
			g.mod = st.ModTime()
		}
		return true, nil
	}
	// Rewritten, not appended to: start trusting the file as it is now, and say so.
	g.size, g.sum = int64(len(data)), sha256.New()
	g.sum.Write(data)
	if st, err := os.Stat(p); err == nil {
		g.mod = st.ModTime()
	}
	return false, fmt.Errorf("chats/%s.jsonl was changed outside Artus", id)
}

func sha(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// restoreChats repairs the chat files this process wrote and returns the
// names it repaired or found changed.
func (h *Home) restoreChats() []string {
	h.chatMu.Lock()
	defer h.chatMu.Unlock()
	var out []string
	for id, g := range h.chats {
		if g.meta != nil {
			p := h.Path("chats", id+".json")
			if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, g.meta) {
				if writeAtomic(p, g.meta, 0o600) == nil {
					out = append(out, "chats/"+id+".json")
				}
			}
		}
		if restored, err := h.checkTranscript(id, g); restored || err != nil {
			out = append(out, "chats/"+id+".jsonl")
		}
	}
	return out
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
	h.chatMu.Lock()
	delete(h.chats, id)
	h.chatMu.Unlock()
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
	h.chatMu.Lock()
	defer h.chatMu.Unlock()
	g := h.chatGuardFor(id)
	h.checkTranscript(id, g)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	f.Close()
	if err != nil {
		return err
	}
	if g.size < 0 { // first write of this process: take the file as it is
		data, _ := os.ReadFile(p)
		g.size, g.sum = int64(len(data)), sha256.New()
		g.sum.Write(data)
	} else {
		g.size += int64(buf.Len())
		g.sum.Write(buf.Bytes())
	}
	if st, err := os.Stat(p); err == nil {
		g.mod = st.ModTime()
	}
	return nil
}

// Records returns every record of a chat's transcript in order.
func (h *Home) Records(id string) ([]json.RawMessage, error) {
	p, err := h.chatPath(id, ".jsonl")
	if err != nil {
		return nil, err
	}
	h.chatMu.Lock()
	if g := h.chats[id]; g != nil {
		h.checkTranscript(id, g)
	}
	h.chatMu.Unlock()
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
