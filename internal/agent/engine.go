// Package agent is the harness core: the model loop, context assembly and
// compaction, the built-in tools, sub-agents and unattended task runs.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/sched"
	"github.com/pschachhuber/artus/internal/secrets"
	"github.com/pschachhuber/artus/internal/store"
)

// Engine runs conversations. One run is active per chat at most; messages
// sent meanwhile are queued and delivered at the next tool boundary.
type Engine struct {
	Home    *store.Home
	Secrets *secrets.Store
	LLM     *llm.Registry
	Sched   *sched.Scheduler
	Prompt  string      // base system prompt
	Emit    func(Event) // receives everything the interface shows live

	// How commands the agent runs reach back into this process (`artus browser …`):
	// BinDir is put first on their PATH and holds an `artus` launcher.
	BinDir, ServerURL, AgentToken string

	mu      sync.Mutex
	runs    map[string]*run
	asks    map[string]chan string     // pending approvals and secret requests
	asking  map[string]chan struct{}   // per conversation: held by the question on screen
	cwds    map[string]string          // working directory per chat
	allowed map[string]map[string]bool // per chat: tools approved "always" in confirm mode
	active  int                        // sub-agents running now
	stopped bool
}

// Event is a live update for the interface.
type Event struct {
	Seq  int64  `json:"seq"`
	Chat string `json:"chat,omitempty"`
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// Record is one line of a chat transcript.
type Record struct {
	T    string    `json:"t"` // system | msg | compact | cut | note
	Time time.Time `json:"time"`
	// system: starts a context segment. The prompt and tool set stay frozen
	// until the next one so the provider's cache and signed reasoning hold.
	System string     `json:"system,omitempty"`
	Tools  []llm.Tool `json:"tools,omitempty"`
	Search *bool      `json:"search,omitempty"` // whether the provider's web search is on
	// cut: the last Drop messages of the segment are void (a stopped compaction).
	Drop int `json:"drop,omitempty"`
	// msg
	Msg   *llm.Message `json:"msg,omitempty"`
	Usage *llm.Usage   `json:"usage,omitempty"`
	// compact: the summary that replaced the previous segment.
	Summary string `json:"summary,omitempty"`
	Tokens  int    `json:"tokens,omitempty"` // context size before compaction
	// note: shown to the user only, never sent to the model.
	Note  string `json:"note,omitempty"`
	Level string `json:"level,omitempty"` // info | error
}

// Input is something said to a chat.
type Input struct {
	Text      string       `json:"text"`
	Files     []Attachment `json:"files,omitempty"`
	Synthetic bool         `json:"-"` // written by the harness (wake-ups), not the user
}

// Attachment is an uploaded file stored under uploads/.
type Attachment struct {
	Path string `json:"path"` // absolute
	Name string `json:"name"`
	Type string `json:"type"` // media type
	Size int64  `json:"size"`
}

// Live is the in-flight state of a run, kept so a reconnecting interface can
// rebuild what it missed.
type Live struct {
	State    string     `json:"state"` // thinking | writing | tool | compacting | waiting | retrying
	Detail   string     `json:"detail,omitempty"`
	Since    time.Time  `json:"since"`
	Started  time.Time  `json:"started"`
	Turn     int        `json:"turn"`
	Text     string     `json:"text,omitempty"`
	Thinking string     `json:"thinking,omitempty"`
	Tools    []LiveTool `json:"tools,omitempty"`
	Queue    []string   `json:"queue,omitempty"`
	Ask      *Ask       `json:"ask,omitempty"`
}

// LiveTool is a tool call that is running or just finished.
type LiveTool struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	Done   bool            `json:"done,omitempty"`
}

// Ask is a question the run is blocked on.
type Ask struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"` // approval | secret
	Tool        string          `json:"tool,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
}

type run struct {
	chat   string
	root   string // the top-level chat whose user answers approvals
	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	live       Live
	queue      []Input
	spent      int64  // tokens processed by this run
	lastText   string // final assistant text, the result of a sub-agent
	compacting bool
}

// New returns an engine. Emit and the server fields are filled in by the caller.
func New(home *store.Home, sec *secrets.Store, reg *llm.Registry, prompt string) *Engine {
	sec.Hide("ARTUS_TOKEN", home.Config().Server.Token) // the model never reads it, wherever it turns up
	return &Engine{
		Home: home, Secrets: sec, LLM: reg, Prompt: prompt,
		Emit: func(Event) {},
		runs: map[string]*run{},
		asks: map[string]chan string{}, asking: map[string]chan struct{}{},
		cwds: map[string]string{}, allowed: map[string]map[string]bool{},
	}
}

func (e *Engine) emit(chat, typ string, data any) {
	e.Emit(Event{Chat: chat, Type: typ, Data: data})
}

// NewChat creates an empty chat using the configured default model.
func (e *Engine) NewChat(meta store.ChatMeta) (store.ChatMeta, error) {
	cfg := e.Home.Config()
	if meta.Model == "" {
		meta.Model = cfg.Model
	}
	if meta.Effort == "" {
		meta.Effort = cfg.Effort
	}
	meta, err := e.Home.NewChat(meta)
	if err == nil {
		e.emit(meta.ID, "chat", meta)
	}
	return meta, err
}

// Send delivers input to a chat. If the chat is busy the input is queued.
func (e *Engine) Send(chatID string, in Input) error {
	if strings.TrimSpace(in.Text) == "" && len(in.Files) == 0 {
		return errors.New("empty message")
	}
	meta, err := e.Home.Chat(chatID)
	if err != nil {
		return fmt.Errorf("no such chat: %s", chatID)
	}
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return errors.New("shutting down")
	}
	if r := e.runs[chatID]; r != nil {
		// Queued under both locks, so the message is either seen by the run or
		// picked up by finish: it cannot fall between a run ending and the next one.
		r.mu.Lock()
		r.queue = append(r.queue, in)
		r.live.Queue = append(r.live.Queue, in.Text)
		r.mu.Unlock()
		e.mu.Unlock()
		e.emitLive(r)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{chat: chatID, root: chatID, cancel: cancel, done: make(chan struct{})}
	r.live.Started = time.Now()
	e.runs[chatID] = r
	e.mu.Unlock()
	go func() {
		defer e.finish(r)
		e.execute(ctx, r, meta, in)
	}()
	return nil
}

func (e *Engine) finish(r *run) {
	r.cancel()
	e.mu.Lock()
	delete(e.runs, r.chat)
	r.mu.Lock()
	late := r.queue // sent after the run's last look at its queue
	r.queue = nil
	r.mu.Unlock()
	stopped := e.stopped
	e.mu.Unlock()
	close(r.done)
	e.emit(r.chat, "live", nil)
	if !stopped {
		for _, in := range late {
			e.Send(r.chat, in)
		}
	}
}

// Stop cancels the run of a chat, if any, and waits for it to end.
func (e *Engine) Stop(chatID string) {
	e.mu.Lock()
	r := e.runs[chatID]
	e.mu.Unlock()
	if r == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
	}
}

// Shutdown stops every run.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	e.stopped = true
	var ids []string
	for id := range e.runs {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.Stop(id)
	}
}

// Live returns the in-flight state of a chat, or nil when it is idle.
func (e *Engine) Live(chatID string) *Live {
	e.mu.Lock()
	r := e.runs[chatID]
	e.mu.Unlock()
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.live
	l.Tools = append([]LiveTool(nil), r.live.Tools...)
	l.Queue = append([]string(nil), r.live.Queue...)
	return &l
}

// Running lists the chats with an active run.
func (e *Engine) Running() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.runs))
	for id := range e.runs {
		ids = append(ids, id)
	}
	return ids
}

func (e *Engine) emitLive(r *run) {
	e.emit(r.chat, "live", e.Live(r.chat))
}

func (e *Engine) setState(r *run, state, detail string) {
	r.mu.Lock()
	r.live.State, r.live.Detail, r.live.Since = state, detail, time.Now()
	r.mu.Unlock()
	e.emitLive(r)
}

// record appends to the transcript and tells the interface.
func (e *Engine) record(chatID string, rec Record) {
	rec.Time = time.Now()
	if err := e.Home.Append(chatID, rec); err != nil {
		e.emit(chatID, "alert", "could not write transcript: "+err.Error())
	}
	e.emit(chatID, "record", rec)
}

func (e *Engine) note(chatID, level, text string) {
	e.record(chatID, Record{T: "note", Note: text, Level: level})
}

// ask blocks the run on a question for the user and returns the answer, or
// "" if the run was stopped first.
func (e *Engine) ask(ctx context.Context, r *run, a Ask) string {
	// One question at a time per conversation: parallel sub-agents take turns.
	e.mu.Lock()
	turn := e.asking[r.root]
	if turn == nil {
		turn = make(chan struct{}, 1)
		e.asking[r.root] = turn
	}
	e.mu.Unlock()
	select {
	case turn <- struct{}{}:
		defer func() { <-turn }()
	case <-ctx.Done():
		return ""
	}

	a.ID = store.RandomID(6)
	ch := make(chan string, 1)
	e.mu.Lock()
	e.asks[a.ID] = ch
	target := e.runs[r.root]
	e.mu.Unlock()
	if target == nil {
		target = r
	}
	target.mu.Lock()
	prev := target.live.State
	target.live.Ask = &a
	target.mu.Unlock()
	e.setState(target, "waiting", a.Kind)
	defer func() {
		e.mu.Lock()
		delete(e.asks, a.ID)
		e.mu.Unlock()
		target.mu.Lock()
		target.live.Ask = nil
		target.mu.Unlock()
		e.setState(target, prev, "")
	}()
	select {
	case ans := <-ch:
		return ans
	case <-ctx.Done():
		return ""
	}
}

// Answer resolves a pending approval or secret request.
func (e *Engine) Answer(id, answer string) bool {
	e.mu.Lock()
	ch := e.asks[id]
	e.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- answer:
		return true
	default:
		return false
	}
}

// parse decodes the records of a chat.
func (e *Engine) records(chatID string) []Record {
	raw, _ := e.Home.Records(chatID)
	out := make([]Record, 0, len(raw))
	for _, line := range raw {
		var rec Record
		if json.Unmarshal(line, &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

// Records returns the transcript of a chat for display.
func (e *Engine) Records(chatID string) []Record { return e.records(chatID) }
