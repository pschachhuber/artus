package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/store"
)

// Section headings of the assembled prompt, also used to size its parts.
const (
	headInstructions = "\n\n# Instructions from the user\n"
	headMemory       = "\n\n# memory.md\n"
	headTopics       = "\n\n# Topics\n"
	headSkills       = "\n\n# Skills\n"
	headSecrets      = "\n\n# Secrets\n"
	headRun          = "\n\n# This run\n"
)

// systemPrompt renders the prompt for a new context segment: the base
// prompt, the user's instructions, memory, and one-line indexes of everything
// that is loaded on demand.
func (e *Engine) systemPrompt(meta store.ChatMeta) string {
	zone, _ := time.Now().Zone()
	var b strings.Builder
	b.WriteString(strings.NewReplacer(
		"{{home}}", e.Home.Dir,
		"{{os}}", runtime.GOOS+"/"+runtime.GOARCH,
		"{{shell}}", shellName(),
		"{{date}}", time.Now().Format("Monday 2 January 2006")+" ("+zone+")",
	).Replace(strings.TrimSpace(e.Prompt)))

	if text := e.readHome(store.InstructionsFile); text != "" {
		b.WriteString(headInstructions + text)
	}
	memory := e.readHome(store.MemoryFile)
	if memory == "" {
		memory = "(empty)"
	}
	b.WriteString(headMemory + memory)

	b.WriteString(headTopics)
	if topics := e.Home.Topics(); len(topics) == 0 {
		b.WriteString("(none yet)")
	} else {
		for _, t := range topics {
			fmt.Fprintf(&b, "- %s — %s\n", t.Path, t.Description)
		}
	}
	b.WriteString(headSkills)
	if skills := e.Home.Skills(); len(skills) == 0 {
		b.WriteString("(none yet)")
	} else {
		for _, s := range skills {
			fmt.Fprintf(&b, "- %s — %s (%s)\n", s.Name, s.Description, s.Path)
		}
	}
	var names []string
	for _, info := range e.Secrets.Names() {
		if !info.Reserved {
			names = append(names, info.Name)
		}
	}
	if len(names) > 0 || e.Home.Config().Secrets.Backend == "protonpass" {
		b.WriteString(headSecrets + strings.Join(names, ", "))
	}
	if e.Home.Config().Secrets.Backend == "protonpass" {
		b.WriteString("\nProton Pass is connected: a secret may be a reference, pass://Vault/Item/field (field: username, password, totp, …), fetched when used. " +
			"`pass-cli item list` shows what the vault holds; register an item with `echo 'pass://Vault/Item/password' | artus secret add NAME`.")
	}

	switch meta.Kind {
	case store.KindTask:
		fmt.Fprintf(&b, headRun+"You are running unattended as task %q (tasks/%s/). Nobody is watching: do not ask questions, decide and act. "+
			"Before you finish, update tasks/%s/NOTES.md with what the next run needs to know, and end with a short report of what you did and found. "+
			"Set status: done in TASK.md when the job is complete for good.", meta.Task, meta.Task, meta.Task)
	case store.KindAgent:
		b.WriteString(headRun + "You are a sub-agent working on one assignment for another agent. Do exactly that assignment. " +
			"Your final message is the only thing it receives, so make it complete and self-contained: findings, file paths, and anything you could not do.")
	}
	return strings.TrimSpace(b.String())
}

func (e *Engine) readHome(rel string) string {
	b, _ := os.ReadFile(e.Home.Path(rel))
	return strings.TrimSpace(string(b))
}

// threshold is the context size at which the chat is compacted.
func (e *Engine) threshold(s *session) int {
	cfg := e.Home.Config().Context
	window := s.meta.Window
	if window == 0 {
		window = cfg.DefaultWindow
	}
	limit := int(float64(window) * cfg.CompactAt)
	if cfg.CompactMaxTokens > 0 && cfg.CompactMaxTokens < limit {
		limit = cfg.CompactMaxTokens
	}
	return limit
}

func (e *Engine) shouldCompact(s *session) bool {
	return len(s.messages) > 2 && s.meta.Context > e.threshold(s)
}

const compactPrompt = `<harness>
This conversation is about to be compacted: everything above will be replaced by the summary you write now.

First, if anything from this conversation should outlive it and is not saved yet (a fact about the user for memory.md, something learned for a topic note, a procedure for a skill), save it now with your tools. Skip this if there is nothing.

Then reply with the summary and nothing else. Write it for yourself, picking the work back up with no other memory of it:

## Goal
What the user wants, in their own words where short. Include constraints and preferences they stated.

## State
What is done, what is in progress, what failed and why.

## Facts and decisions
Specific values, names, IDs, commands and conclusions that took effort to find. Decisions made and the reasons.

## Files and places
Paths, URLs and other locations touched or relevant, with what each is.

## Next
%s
</harness>`

// compact replaces the context with a summary written by the model. The
// transcript on disk is untouched; only what is sent from now on changes.
// Nothing from the old segment is replayed, which keeps the history
// append-only for providers that sign their reasoning against it.
func (e *Engine) compact(ctx context.Context, r *run, s *session, midTask bool) error {
	r.mu.Lock()
	busy := r.compacting
	r.compacting = true
	r.mu.Unlock()
	if busy {
		return nil
	}
	defer func() {
		r.mu.Lock()
		r.compacting = false
		r.mu.Unlock()
	}()
	e.setState(r, "compacting", "")
	before := s.meta.Context

	next := "The exact next step, concrete enough to start immediately."
	if !midTask {
		next = "Anything left open or promised for later."
	}
	e.append(s, llm.Message{Role: "user", Synthetic: true, Blocks: []llm.Block{{Type: llm.Text, Text: fmt.Sprintf(compactPrompt, next)}}}, nil)

	summary := ""
	for round := 0; round < 8 && summary == ""; round++ {
		resp, err := e.request(ctx, r, s)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			summary = digest(s) // the model could not be reached or the context no longer fits
			break
		}
		resp.Message.Model = s.meta.Model
		e.append(s, resp.Message, &resp.Usage)
		e.account(r, s, resp.Usage)
		if calls := resp.Message.Calls(); len(calls) > 0 {
			e.append(s, llm.Message{Role: "user", Blocks: e.runTools(ctx, r, s, calls)}, nil)
			continue
		}
		summary = strings.TrimSpace(resp.Message.TextOf())
	}
	if summary == "" {
		summary = digest(s)
	}

	e.record(s.meta.ID, Record{T: "compact", Summary: summary, Tokens: before})
	e.begin(s)
	text := "<summary>\n" + summary + "\n</summary>\n\n<harness>The conversation so far was compacted into the summary above. " +
		"The full transcript is in chats/" + s.meta.ID + ".jsonl; grep it if you need an exact detail."
	if midTask {
		text += " Continue the work from where it stopped."
	}
	text += "</harness>"
	e.append(s, llm.Message{Role: "user", Synthetic: true, Blocks: []llm.Block{{Type: llm.Text, Text: text}}}, nil)
	s.meta.Context = llm.EstimateTokens(llm.Request{System: s.system, Tools: s.tools, Messages: s.messages})
	e.saveMeta(s)
	return nil
}

// digest is the fallback summary when the model cannot write one: the first
// request and the most recent replies, verbatim.
func digest(s *session) string {
	var first, recent []string
	for _, m := range s.messages {
		text := strings.TrimSpace(m.TextOf())
		if text == "" || m.Synthetic {
			continue
		}
		if r := []rune(text); len(r) > 1500 {
			text = string(r[:1500]) + "…"
		}
		if m.Role == "user" && len(first) == 0 {
			first = append(first, text)
		}
		recent = append(recent, m.Role+": "+text)
	}
	if len(recent) > 6 {
		recent = recent[len(recent)-6:]
	}
	return "(Automatic digest: the model could not summarise.)\n\n## First request\n" + strings.Join(first, "\n") +
		"\n\n## Most recent messages\n" + strings.Join(recent, "\n\n")
}

// Compact compacts an idle chat on request.
func (e *Engine) Compact(chatID string) error {
	meta, err := e.Home.Chat(chatID)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.runs[chatID] != nil {
		e.mu.Unlock()
		return fmt.Errorf("the chat is busy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{chat: chatID, root: chatID, cancel: cancel, done: make(chan struct{})}
	r.live.Started = time.Now()
	e.runs[chatID] = r
	e.mu.Unlock()
	go func() {
		defer e.finish(r)
		s := e.load(meta)
		if len(s.messages) < 2 {
			return
		}
		if err := e.compact(ctx, r, s, false); err != nil {
			e.note(chatID, "error", "Compaction failed: "+err.Error())
		}
	}()
	return nil
}

// ContextInfo is what the context inspector shows.
type ContextInfo struct {
	System    string         `json:"system"`
	Tools     []llm.Tool     `json:"tools"`
	Messages  []llm.Message  `json:"messages"`
	Tokens    int            `json:"tokens"`
	Window    int            `json:"window"`
	Threshold int            `json:"threshold"`
	Breakdown map[string]int `json:"breakdown"` // estimated tokens per part
	Loaded    []string       `json:"loaded"`    // files read into this context
}

// Context describes exactly what the next request of a chat would send.
func (e *Engine) Context(chatID string) (ContextInfo, error) {
	meta, err := e.Home.Chat(chatID)
	if err != nil {
		return ContextInfo{}, err
	}
	s := e.load(meta)
	info := ContextInfo{System: s.system, Tools: s.tools, Messages: s.messages, Tokens: meta.Context, Window: meta.Window, Threshold: e.threshold(s)}
	if info.Window == 0 {
		info.Window = e.Home.Config().Context.DefaultWindow
	}

	chars := map[string]int{}
	rest := s.system
	for _, part := range []struct{ key, head string }{
		{"run", headRun}, {"secrets", headSecrets}, {"skills", headSkills}, {"topics", headTopics},
		{"memory", headMemory}, {"instructions", headInstructions},
	} {
		if i := strings.LastIndex(rest, strings.TrimLeft(part.head, "\n")); i >= 0 {
			key := part.key
			if key == "run" || key == "secrets" {
				key = "prompt"
			}
			if key == "skills" || key == "topics" {
				key = "indexes"
			}
			chars[key] += len(rest) - i
			rest = rest[:i]
		}
	}
	chars["prompt"] += len(rest)
	for _, t := range s.tools {
		chars["tools"] += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	seen := map[string]bool{}
	for _, m := range s.messages {
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.ToolResult:
				chars["toolResults"] += len(b.Text)
			case llm.Image, llm.File:
				chars["attachments"] += 6000 * 4
			case llm.ToolCall:
				chars["messages"] += len(b.Input)
				var in struct {
					Path string `json:"path"`
				}
				if b.Name == "read" && json.Unmarshal(b.Input, &in) == nil && in.Path != "" && !seen[in.Path] {
					seen[in.Path] = true
					info.Loaded = append(info.Loaded, in.Path)
				}
			default:
				chars["messages"] += len(b.Text)
			}
		}
	}
	total := 0
	for _, n := range chars {
		total += n
	}
	info.Breakdown = map[string]int{}
	for k, n := range chars {
		tokens := n / 4
		if info.Tokens > 0 && total > 0 {
			tokens = int(float64(n) / float64(total) * float64(info.Tokens))
		}
		info.Breakdown[k] = tokens
	}
	if info.Tokens == 0 {
		info.Tokens = total / 4
	}
	return info, nil
}
