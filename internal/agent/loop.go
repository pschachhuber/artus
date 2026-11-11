package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/store"
)

// session is the live context of one chat: everything since its last
// "system" record. It only ever grows; compaction starts a new one.
type session struct {
	meta     store.ChatMeta
	system   string
	tools    []llm.Tool
	messages []llm.Message
}

// load rebuilds the session from the transcript.
func (e *Engine) load(meta store.ChatMeta) *session {
	s := &session{meta: meta}
	for _, rec := range e.records(meta.ID) {
		switch rec.T {
		case "system":
			s.system, s.tools, s.messages = rec.System, rec.Tools, nil
		case "msg":
			if rec.Msg != nil {
				s.messages = append(s.messages, *rec.Msg)
			}
		}
	}
	return s
}

// begin starts a new context segment with a freshly rendered prompt.
func (e *Engine) begin(s *session) {
	s.system = e.systemPrompt(s.meta)
	s.tools = e.toolDefs(s.meta)
	s.messages = nil
	e.record(s.meta.ID, Record{T: "system", System: s.system, Tools: s.tools})
}

func (e *Engine) append(s *session, msg llm.Message, usage *llm.Usage) {
	s.messages = append(s.messages, msg)
	e.record(s.meta.ID, Record{T: "msg", Msg: &msg, Usage: usage})
}

// userMessage turns input into a message: a timestamp, the text, and
// attachments as native blocks where the format allows plus their paths.
func userMessage(in Input, home *store.Home) llm.Message {
	msg := llm.Message{Role: "user", Synthetic: in.Synthetic}
	msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.Text, Text: "<time>" + time.Now().Format("2006-01-02 15:04 Mon") + "</time>"})
	var paths []string
	for _, f := range in.Files {
		rel := f.Path
		if r, ok := home.Rel(f.Path); ok {
			rel = r
		}
		paths = append(paths, fmt.Sprintf("- %s (%s, %d bytes)", rel, f.Type, f.Size))
		switch {
		case strings.HasPrefix(f.Type, "image/"):
			msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.Image, Path: f.Path, MediaType: f.Type, Name: f.Name})
		case f.Type == "application/pdf":
			msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.File, Path: f.Path, MediaType: f.Type, Name: f.Name})
		}
	}
	text := in.Text
	if len(paths) > 0 {
		text = strings.TrimSpace(text + "\n\n<attachments>\n" + strings.Join(paths, "\n") + "\n</attachments>")
	}
	msg.Blocks = append(msg.Blocks, llm.Block{Type: llm.Text, Text: text})
	return msg
}

// execute runs a chat from one input until the model stops asking for tools
// and nothing is queued.
func (e *Engine) execute(ctx context.Context, r *run, meta store.ChatMeta, in Input) {
	s := e.load(meta)
	if s.system == "" {
		e.begin(s)
	}
	if s.meta.Title == "" {
		s.meta.Title = title(in.Text)
	}
	e.append(s, userMessage(in, e.Home), nil)
	e.saveMeta(s)

	cfg := e.Home.Config()
	continued := 0
	for turn := 1; ; turn++ {
		if ctx.Err() != nil {
			e.note(meta.ID, "info", "Stopped.")
			break
		}
		if turn > cfg.Limits.Turns {
			e.note(meta.ID, "error", fmt.Sprintf("Stopped after %d model requests (limits.turns).", cfg.Limits.Turns))
			break
		}
		if meta.Kind == store.KindTask && cfg.Limits.TaskTokens > 0 && r.spent > cfg.Limits.TaskTokens {
			e.note(meta.ID, "error", fmt.Sprintf("Stopped: this run used its token budget of %d (limits.taskTokens).", cfg.Limits.TaskTokens))
			break
		}
		r.mu.Lock()
		r.live.Turn, r.live.Text, r.live.Thinking, r.live.Tools = turn, "", "", nil
		r.mu.Unlock()

		resp, err := e.request(ctx, r, s)
		if err != nil {
			if ctx.Err() != nil {
				e.note(meta.ID, "info", "Stopped.")
			} else {
				e.note(meta.ID, "error", err.Error())
			}
			break
		}
		resp.Message.Model = s.meta.Model
		e.append(s, resp.Message, &resp.Usage)
		e.account(r, s, resp.Usage)
		r.mu.Lock()
		r.lastText = resp.Message.TextOf()
		r.mu.Unlock()

		calls := resp.Message.Calls()
		switch {
		case len(calls) > 0:
			results := e.runTools(ctx, r, s, calls)
			if restored := e.Home.Restore(); len(restored) > 0 {
				warn := strings.Join(restored, ", ") + " belongs to the user and was changed outside Settings; it has been restored. Do not modify it by any means: tell the user what you wanted to change and that they can change it in Settings."
				results = append(results, llm.Block{Type: llm.Text, Text: "<harness>" + warn + "</harness>"})
				e.emit(meta.ID, "alert", warn)
			}
			e.append(s, llm.Message{Role: "user", Blocks: results}, nil)
		case resp.Stop == llm.StopRefusal:
			e.note(meta.ID, "error", "The model declined to continue. "+resp.Detail)
		case resp.Stop == llm.StopLength && continued < 2:
			continued++
			e.append(s, llm.Message{Role: "user", Synthetic: true, Blocks: []llm.Block{{Type: llm.Text, Text: "<harness>Your reply hit the output limit. Continue exactly where it stopped.</harness>"}}}, nil)
			continue
		}

		queued := e.drain(r)
		for _, q := range queued {
			e.append(s, userMessage(q, e.Home), nil)
		}
		if len(calls) == 0 && len(queued) == 0 {
			break
		}
		if e.shouldCompact(s) {
			if err := e.compact(ctx, r, s, true); err != nil && ctx.Err() == nil {
				e.note(meta.ID, "error", "Compaction failed: "+err.Error())
			}
		}
	}
	if ctx.Err() == nil && e.shouldCompact(s) {
		if err := e.compact(ctx, r, s, false); err != nil && ctx.Err() == nil {
			e.note(meta.ID, "error", "Compaction failed: "+err.Error())
		}
	}
	if meta.Kind != store.KindAgent && e.Home.Snapshot(s.meta.Title) {
		e.emit("", "changes", nil)
	}
}

func title(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > 60 {
		return strings.TrimSpace(string(r[:60])) + "…"
	}
	if text == "" {
		return "New chat"
	}
	return text
}

func (e *Engine) drain(r *run) []Input {
	r.mu.Lock()
	queued := r.queue
	r.queue, r.live.Queue = nil, nil
	r.mu.Unlock()
	return queued
}

// saveMeta persists chat metadata, keeping flags the interface may have
// changed meanwhile (title, pin, model).
func (e *Engine) saveMeta(s *session) {
	if cur, err := e.Home.Chat(s.meta.ID); err == nil {
		s.meta.Pinned, s.meta.Unread = cur.Pinned, cur.Unread
		if cur.Title != "" {
			s.meta.Title = cur.Title
		}
		s.meta.Model, s.meta.Effort = cur.Model, cur.Effort
	}
	s.meta.Updated = time.Now()
	e.Home.SaveChat(s.meta)
	e.emit(s.meta.ID, "chat", s.meta)
}

func (e *Engine) account(r *run, s *session, u llm.Usage) {
	s.meta.Context = u.Input + u.Output
	s.meta.TokensIn += int64(u.Input)
	s.meta.TokensOut += int64(u.Output)
	s.meta.CacheRead += int64(u.CacheRead)
	s.meta.Cost += u.Cost
	r.mu.Lock()
	r.spent += int64(u.Input + u.Output)
	r.mu.Unlock()
	e.saveMeta(s)
}

// request makes one model call, streaming fragments to the interface and
// retrying transient failures.
func (e *Engine) request(ctx context.Context, r *run, s *session) (*llm.Response, error) {
	provider, model, err := e.LLM.Resolve(s.meta.Model)
	if err != nil {
		return nil, fmt.Errorf("%w. Choose a model in Settings", err)
	}
	if s.meta.Window == 0 {
		s.meta.Window = e.Home.Config().Context.DefaultWindow
		if info, ok := e.LLM.Info(ctx, s.meta.Model); ok && info.Window > 0 {
			s.meta.Window = info.Window
		}
	}
	req := llm.Request{
		ID: s.meta.Model, Model: model, Effort: s.meta.Effort, System: s.system, Messages: s.messages,
		Tools: s.tools, WebSearch: e.Home.Config().WebSearch, Cache: s.meta.ID,
	}
	emit := func(d llm.Delta) {
		r.mu.Lock()
		switch d.Kind {
		case llm.Text:
			r.live.Text += d.Text
			r.live.State = "writing"
		case llm.Thinking:
			r.live.Thinking += d.Text
		case "tool":
			if d.Name != "" {
				r.live.Tools = append(r.live.Tools, LiveTool{ID: d.ID, Name: d.Name})
			}
		}
		r.mu.Unlock()
		e.emit(r.chat, "delta", d)
	}
	compacted := false
	for attempt := 0; ; attempt++ {
		e.setState(r, "thinking", "")
		resp, err := provider.Stream(ctx, req, emit)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var perr *llm.Error
		if errors.As(err, &perr) {
			if overflow(perr) && !compacted && len(s.messages) > 2 {
				compacted = true
				if cerr := e.compact(ctx, r, s, true); cerr == nil {
					req.System, req.Messages, req.Tools = s.system, s.messages, s.tools
					continue
				}
			}
			if perr.Retryable() && attempt < 4 {
				wait := []time.Duration{2, 5, 15, 30}[attempt] * time.Second
				e.setState(r, "retrying", perr.Error())
				r.mu.Lock()
				r.live.Text, r.live.Thinking, r.live.Tools = "", "", nil
				r.mu.Unlock()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(wait):
				}
				continue
			}
		}
		return nil, err
	}
}

// overflow recognises "the prompt does not fit" errors across providers.
func overflow(err *llm.Error) bool {
	if err.Status != 400 && err.Status != 413 {
		return false
	}
	m := strings.ToLower(err.Message)
	for _, hint := range []string{"too long", "context length", "context window", "maximum context", "too many tokens", "exceeds the limit"} {
		if strings.Contains(m, hint) {
			return true
		}
	}
	return false
}

// runTools executes the calls of one assistant turn and returns their result
// blocks in call order. Calls run concurrently when every one is safe to.
func (e *Engine) runTools(ctx context.Context, r *run, s *session, calls []llm.Block) []llm.Block {
	tools := e.tools(s.meta)
	parallel := len(calls) > 1
	for _, c := range calls {
		if t, ok := tools[c.Name]; !ok || !t.Parallel {
			parallel = false
		}
	}
	r.mu.Lock()
	r.live.Tools = r.live.Tools[:0]
	for _, c := range calls {
		r.live.Tools = append(r.live.Tools, LiveTool{ID: c.ID, Name: c.Name, Input: c.Input})
	}
	r.mu.Unlock()

	out := make([][]llm.Block, len(calls))
	one := func(i int) {
		c := calls[i]
		e.setState(r, "tool", c.Name)
		started := time.Now()
		res := e.runTool(ctx, r, s, tools, c)
		text := e.Secrets.Redact(res.Text)
		text = e.capOutput(text)
		ui, _ := json.Marshal(map[string]any{"ms": time.Since(started).Milliseconds(), "ui": res.UI})
		out[i] = append([]llm.Block{{Type: llm.ToolResult, ID: c.ID, Name: c.Name, Text: text, IsError: res.IsError, UI: ui}}, res.Blocks...)
		r.mu.Lock()
		for j := range r.live.Tools {
			if r.live.Tools[j].ID == c.ID {
				r.live.Tools[j].Done = true
			}
		}
		r.mu.Unlock()
		e.emitLive(r)
	}
	if parallel {
		var wg sync.WaitGroup
		for i := range calls {
			wg.Add(1)
			go func() { defer wg.Done(); one(i) }()
		}
		wg.Wait()
	} else {
		for i := range calls {
			one(i)
		}
	}
	// Tool results first, then any images they produced: providers require
	// results to lead the message.
	var results, extras []llm.Block
	for _, blocks := range out {
		results = append(results, blocks[0])
		extras = append(extras, blocks[1:]...)
	}
	return append(results, extras...)
}

func (e *Engine) runTool(ctx context.Context, r *run, s *session, tools map[string]*Tool, c llm.Block) (res Result) {
	t, ok := tools[c.Name]
	if !ok {
		return Result{Text: "Unknown tool " + c.Name + ".", IsError: true}
	}
	if !json.Valid(c.Input) || !strings.HasPrefix(strings.TrimSpace(string(c.Input)), "{") {
		return Result{Text: "The arguments were cut off or are not a JSON object, so nothing was run. Call the tool again with complete arguments; for a large file, write it in parts.", IsError: true}
	}
	if ctx.Err() != nil {
		return Result{Text: "Stopped by the user.", IsError: true}
	}
	if t.Risky && e.needsApproval(r, s.meta, c.Name) {
		switch e.ask(ctx, r, Ask{Kind: "approval", Tool: c.Name, Input: c.Input}) {
		case "allow":
		case "always":
			r.mu.Lock()
			r.allowed[c.Name] = true
			r.mu.Unlock()
		default:
			return Result{Text: "The user declined this action. Do not retry it; ask what they would like instead.", IsError: true}
		}
	}
	defer func() {
		if p := recover(); p != nil {
			res = Result{Text: fmt.Sprintf("The tool crashed: %v", p), IsError: true}
		}
	}()
	call := &Call{e: e, r: r, Chat: s.meta, ID: c.ID, Input: c.Input}
	return t.Run(ctx, call)
}

// needsApproval is true for risky tools in "confirm" mode. Unattended task
// runs never ask: nobody is there to answer.
func (e *Engine) needsApproval(r *run, meta store.ChatMeta, tool string) bool {
	if e.Home.Config().Autonomy != "confirm" || meta.Kind == store.KindTask {
		return false
	}
	e.mu.Lock()
	root := e.runs[r.root]
	e.mu.Unlock()
	if root == nil {
		root = r
	}
	if rootMeta, err := e.Home.Chat(root.chat); err == nil && rootMeta.Kind == store.KindTask {
		return false
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	return !root.allowed[tool]
}

// capOutput keeps a tool result within the context budget, saving the full
// text to a file the agent can page through.
func (e *Engine) capOutput(text string) string {
	limit := e.Home.Config().Context.ToolOutputChars
	if limit <= 0 || len(text) <= limit {
		return text
	}
	path := e.Home.Path("tmp", "output-"+store.RandomID(4)+".txt")
	if err := writeFile(path, text); err != nil {
		return text[:limit] + "\n… (truncated)"
	}
	rel, _ := e.Home.Rel(path)
	head, tail := limit*2/3, limit/3
	for head > 0 && !utf8Start(text[head]) {
		head--
	}
	cut := len(text) - tail
	for cut < len(text) && !utf8Start(text[cut]) {
		cut++
	}
	return fmt.Sprintf("%s\n\n… %d characters omitted. Full output (%d lines): %s — read it with offset/limit or grep it.\n\n%s",
		text[:head], cut-head, strings.Count(text, "\n")+1, filepath.ToSlash(rel), text[cut:])
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
