package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/store"
)

// Tool is something the model can call.
type Tool struct {
	Def      llm.Tool
	Parallel bool // safe to run alongside other calls of the same turn
	Risky    bool // changes things: asks first when autonomy is "confirm"
	Run      func(ctx context.Context, c *Call) Result
}

// Call is one invocation of a tool.
type Call struct {
	e     *Engine
	r     *run
	Chat  store.ChatMeta
	ID    string
	Input json.RawMessage
}

// Result is what a tool hands back to the model.
type Result struct {
	Text    string
	IsError bool
	Blocks  []llm.Block // images or files to show the model alongside the text
	UI      any         // display-only details for the interface
}

func fail(format string, args ...any) Result {
	return Result{Text: fmt.Sprintf(format, args...), IsError: true}
}

// args decodes the call's arguments.
func (c *Call) args(v any) error {
	if err := json.Unmarshal(c.Input, v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

// output streams partial output of a running tool to the interface.
func (c *Call) output(chunk string) {
	c.r.mu.Lock()
	for i := range c.r.live.Tools {
		if t := &c.r.live.Tools[i]; t.ID == c.ID {
			t.Output += chunk
			if len(t.Output) > 20000 {
				t.Output = t.Output[len(t.Output)-16000:]
			}
		}
	}
	c.r.mu.Unlock()
	c.e.emit(c.r.chat, "tool_output", map[string]string{"id": c.ID, "text": chunk})
}

// path resolves a path the model gave: ~ is the user's home directory and
// relative paths start at the chat's working directory.
func (c *Call) path(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.e.cwd(c.Chat.ID), p)
	}
	return filepath.Clean(p)
}

// cwd returns the working directory of a chat: the Artus home until a shell
// command changes it.
func (e *Engine) cwd(chatID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if dir := e.cwds[chatID]; dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
	}
	return e.Home.Dir
}

func (e *Engine) setCwd(chatID, dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cwds[chatID] = dir
}

// display shortens a path for messages: relative to the Artus home when inside it.
func (e *Engine) display(p string) string {
	if rel, ok := e.Home.Rel(p); ok {
		return rel
	}
	return filepath.ToSlash(p)
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

func writeFile(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o600)
}

// builtins lists the built-in tools in the order they are declared to the model.
func (e *Engine) builtins() []*Tool {
	return []*Tool{bashTool, readTool, writeTool, editTool, globTool, grepTool, fetchTool, agentTool, scheduleTool, secretTool}
}

// tools returns every tool available to a chat, by name.
func (e *Engine) tools(meta store.ChatMeta) map[string]*Tool {
	out := map[string]*Tool{}
	for _, t := range e.builtins() {
		if t == agentTool && meta.Depth >= e.Home.Config().Limits.Depth {
			continue // deepest level: no further delegation
		}
		out[t.Def.Name] = t
	}
	for _, ext := range e.Home.ExternalTools() {
		if _, taken := out[ext.Name]; !taken {
			out[ext.Name] = externalTool(ext)
		}
	}
	return out
}

// toolDefs is the declaration sent to the model: built-ins in fixed order,
// then the agent's own tools by name, so the list is stable between requests.
func (e *Engine) toolDefs(meta store.ChatMeta) []llm.Tool {
	tools := e.tools(meta)
	var defs []llm.Tool
	for _, t := range e.builtins() {
		if _, ok := tools[t.Def.Name]; ok {
			defs = append(defs, t.Def)
			delete(tools, t.Def.Name)
		}
	}
	var names []string
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		defs = append(defs, tools[name].Def)
	}
	return defs
}
