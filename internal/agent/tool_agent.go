package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/sched"
	"github.com/pschachhuber/artus/internal/store"
)

var agentTool = &Tool{
	Parallel: true,
	Def: llm.Tool{
		Name: "agent",
		Description: "Hand a self-contained piece of work to a sub-agent with a fresh context and the same tools, and get back its final report. " +
			"Use it to keep bulky reading or searching out of your own context, and call it several times in one turn to work in parallel. " +
			"The sub-agent knows nothing of this conversation: give it the goal, what you already know, the files or URLs to look at, and what to report back. " +
			"Optionally pick a model from the allowlist, such as a cheaper one for simple lookups.",
		Schema: schema(`{"type":"object","properties":{
			"prompt":{"type":"string","description":"The complete assignment."},
			"name":{"type":"string","description":"A short label shown to the user."},
			"model":{"type":"string","description":"provider/model from the allowlist. Default: your own model."},
			"effort":{"type":"string","enum":["low","medium","high","xhigh","max"]}
		},"required":["prompt"]}`),
	},
}

// Assigned at start-up: running a sub-agent re-enters the loop, which needs the tool list.
func init() { agentTool.Run = runAgent }

func runAgent(ctx context.Context, c *Call) Result {
	var in struct{ Prompt, Name, Model, Effort string }
	if err := c.args(&in); err != nil {
		return fail("%v", err)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return fail("prompt is empty")
	}
	cfg := c.e.Home.Config()
	if in.Model == "" {
		in.Model = c.Chat.Model
	} else if in.Model != c.Chat.Model && in.Model != cfg.Model && !contains(cfg.AllowedModels, in.Model) {
		return fail("%s is not on the allowlist. Allowed: %s", in.Model, strings.Join(append([]string{cfg.Model}, cfg.AllowedModels...), ", "))
	}
	if in.Effort == "" {
		in.Effort = c.Chat.Effort
	}
	if in.Name == "" {
		in.Name = title(in.Prompt)
	}
	text, child, err := c.e.spawn(ctx, c.r, c.Chat, in.Name, in.Prompt, in.Model, in.Effort)
	ui := map[string]any{"chat": child, "name": in.Name, "model": in.Model}
	if err != nil {
		return Result{Text: err.Error(), IsError: true, UI: ui}
	}
	return Result{Text: text, UI: ui}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// spawn runs a sub-agent to completion in a child chat and returns its final message.
func (e *Engine) spawn(ctx context.Context, parent *run, parentMeta store.ChatMeta, name, prompt, model, effort string) (string, string, error) {
	limit := max(e.Home.Config().Limits.Subagents, 1)
	e.mu.Lock()
	for e.active >= limit {
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		e.mu.Lock()
	}
	e.active++
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.active--
		e.mu.Unlock()
	}()

	meta, err := e.NewChat(store.ChatMeta{
		Kind: store.KindAgent, Title: name, Model: model, Effort: effort,
		Parent: parentMeta.ID, Depth: parentMeta.Depth + 1,
	})
	if err != nil {
		return "", "", err
	}
	childCtx, cancel := context.WithCancel(ctx)
	r := &run{chat: meta.ID, root: parent.root, cancel: cancel, done: make(chan struct{})}
	r.live.Started = time.Now()
	e.mu.Lock()
	e.runs[meta.ID] = r
	e.mu.Unlock()
	e.execute(childCtx, r, meta, Input{Text: prompt, Synthetic: true})
	e.finish(r)

	parent.mu.Lock()
	parent.spent += r.spent
	parent.mu.Unlock()
	if ctx.Err() != nil {
		return "", meta.ID, ctx.Err()
	}
	if strings.TrimSpace(r.lastText) == "" {
		return "", meta.ID, fmt.Errorf("the sub-agent ended without a report; its transcript is chats/%s.jsonl", meta.ID)
	}
	return r.lastText, meta.ID, nil
}

var scheduleTool = &Tool{
	Def: llm.Tool{
		Name: "schedule",
		Description: "Make something happen later or repeatedly. Without a target, the trigger wakes this conversation and hands you your note: use that for reminders and follow-ups. " +
			"With target \"task:<id>\" it starts an unattended run of that task (tasks/<id>/TASK.md must exist) in a fresh context. " +
			"when is one of: \"cron 0 7 * * 1-5\" (minute hour day month weekday, local time), \"every 2h\", \"in 30m\", \"at 2026-12-01T09:00\", \"now\". " +
			"Units: m, h, d, w. Use list to see triggers and cancel with the id it shows.",
		Schema: schema(`{"type":"object","properties":{
			"action":{"type":"string","enum":["set","list","cancel"]},
			"when":{"type":"string","description":"For set."},
			"target":{"type":"string","description":"For set and list: \"task:<id>\" or \"chat\" (this conversation, the default)."},
			"note":{"type":"string","description":"For set: what to do when it fires. You will read this with no other reminder."},
			"id":{"type":"string","description":"For cancel."}
		},"required":["action"]}`),
	},
	Run: func(_ context.Context, c *Call) Result {
		var in struct{ Action, When, Target, Note, ID string }
		if err := c.args(&in); err != nil {
			return fail("%v", err)
		}
		s := c.e.Sched
		if s == nil {
			return fail("the scheduler is not running")
		}
		target := in.Target
		switch {
		case target == "" || target == "chat":
			target = "chat:" + c.Chat.ID
			if c.Chat.Kind == store.KindTask {
				target = "task:" + c.Chat.Task
			}
		case strings.HasPrefix(target, "task:"):
			if _, err := c.e.Home.Task(strings.TrimPrefix(target, "task:")); err != nil {
				return fail("There is no %s. Create tasks/%s/TASK.md first.", target, strings.TrimPrefix(target, "task:"))
			}
		default:
			return fail(`target must be "chat" or "task:<id>"`)
		}
		switch in.Action {
		case "set":
			if c.Chat.Kind == store.KindAgent && strings.HasPrefix(target, "chat:") {
				return fail("A sub-agent cannot be woken later. Schedule a task instead, or report back that a follow-up is needed.")
			}
			t, err := s.Add(target, in.When, in.Note)
			if err != nil {
				return fail("%v", err)
			}
			c.e.emit("", "schedule", nil)
			return Result{Text: fmt.Sprintf("Trigger %s set for %s: %s. Next: %s.", t.ID, target, sched.Describe(t.When), t.Next.Format("Mon 2 Jan 2006 15:04"))}
		case "list":
			if in.Target == "" {
				target = ""
			}
			list := s.List(target)
			if len(list) == 0 {
				return Result{Text: "No triggers."}
			}
			var b strings.Builder
			for _, t := range list {
				fmt.Fprintf(&b, "%s  %s  %s  next %s", t.ID, t.Target, sched.Describe(t.When), t.Next.Format("2006-01-02 15:04"))
				if t.Note != "" {
					b.WriteString("  — " + t.Note)
				}
				b.WriteString("\n")
			}
			return Result{Text: b.String()}
		case "cancel":
			if !s.Remove(in.ID) {
				return fail("No trigger has the id %q. Use list.", in.ID)
			}
			c.e.emit("", "schedule", nil)
			return Result{Text: "Cancelled " + in.ID + "."}
		}
		return fail("action must be set, list or cancel")
	},
}

var secretTool = &Tool{
	Def: llm.Tool{
		Name: "ask_secret",
		Description: "Ask the user for a secret you need (a password, API key, card number). They type it into a form; it is stored under NAME and you never see it. " +
			"Afterwards use it by name: $NAME in bash, or --secret NAME with `artus browser fill`. Check the Secrets list first, it may already exist. " +
			"To store a value you generated yourself, pipe it in without printing it: `some-command | artus secret add NAME`.",
		Schema: schema(`{"type":"object","properties":{
			"name":{"type":"string","description":"UPPER_SNAKE_CASE name to store it under, for example GITHUB_TOKEN."},
			"description":{"type":"string","description":"One sentence for the user: what it is, where to find it, and what you will use it for."}
		},"required":["name","description"]}`),
	},
	Run: func(ctx context.Context, c *Call) Result {
		var in struct{ Name, Description string }
		if err := c.args(&in); err != nil {
			return fail("%v", err)
		}
		if c.e.Secrets.Has(in.Name) {
			return Result{Text: in.Name + " is already stored. Use it by name."}
		}
		if c.Chat.Kind == store.KindTask || c.e.rootKind(c.r) == store.KindTask {
			return fail("Nobody is here to answer: this is an unattended run. Note in NOTES.md and your report that %s is needed, and continue with what you can do without it.", in.Name)
		}
		value := c.e.ask(ctx, c.r, Ask{Kind: "secret", Name: in.Name, Description: in.Description})
		if value == "" {
			return fail("The user did not provide %s. Ask what they would like to do instead.", in.Name)
		}
		if err := c.e.Secrets.Set(in.Name, value); err != nil {
			return fail("%v", err)
		}
		c.e.emit("", "secrets", nil)
		return Result{Text: in.Name + " is saved. Use it by name; its value will never appear in your context."}
	},
}

func (e *Engine) rootKind(r *run) string {
	if meta, err := e.Home.Chat(r.root); err == nil {
		return meta.Kind
	}
	return ""
}

// externalTool wraps a tool the agent built: its command receives the
// arguments as JSON on stdin and prints the result.
func externalTool(ext store.ExternalTool) *Tool {
	params := ext.Parameters
	if len(params) == 0 || !json.Valid(params) {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return &Tool{
		Risky: true,
		Def:   llm.Tool{Name: ext.Name, Description: ext.Description, Schema: params},
		Run: func(ctx context.Context, c *Call) Result {
			timeout := ext.Timeout
			if timeout <= 0 {
				timeout = 120
			}
			env, _, err := c.e.commandEnv(ctx, c.Chat, strings.Join(ext.Secrets, " "))
			if err != nil {
				return fail("%v", err)
			}
			runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
			defer cancel()
			cmd := exec.Command(ext.Command[0], ext.Command[1:]...)
			cmd.Dir, cmd.Env, cmd.Stdin = ext.Dir, env, bytes.NewReader(c.Input)
			detach(cmd)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				return fail("Could not start tools/%s: %v", ext.Name, err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err = <-done:
			case <-runCtx.Done():
				killTree(cmd)
				<-done
				err = fmt.Errorf("stopped after %ds", timeout)
			}
			text := strings.TrimSpace(strings.ToValidUTF8(out.String(), ""))
			if err != nil {
				hint := fmt.Sprintf("[%v; the command in tools/%s/tool.json runs inside that folder, so its paths are relative to it]", err, filepath.Base(ext.Dir))
				return Result{Text: strings.TrimSpace(text + "\n" + hint), IsError: true}
			}
			if text == "" {
				text = "(no output)"
			}
			return Result{Text: text}
		},
	}
}
