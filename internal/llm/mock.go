package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Mock is a scripted provider for tests and for trying the interface without
// an API key (start Artus with ARTUS_MOCK=1 and pick the model "mock/echo").
//
// Without a script it echoes the last user message. A user message of the
// form "tool <name> <json>" makes it call that tool once and then report the
// result, which is enough to exercise the whole loop by hand. The message
// "demo" walks through a run of several steps (see demo).
type Mock struct {
	mu    sync.Mutex
	Steps []Step             // consumed in order, one per request
	Reply func(Request) Step // when set, decides each reply instead of Steps
	Seen  []Request          // every request received
	Delay time.Duration      // pause between streamed words
}

// Step is one scripted reply.
type Step struct {
	Thinking string
	Text     string
	Calls    []Block // tool calls to make
	Usage    Usage
	Err      error
	Stop     string // overrides the stop reason, e.g. StopRefusal
}

// Models implements Provider.
func (m *Mock) Models(context.Context) ([]Model, error) {
	return []Model{{ID: "echo", Name: "Mock echo", Window: 200_000, Vision: true, Reasoning: true}}, nil
}

// Requests returns a copy of every request received so far.
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.Seen...)
}

// Stream implements Provider.
func (m *Mock) Stream(ctx context.Context, req Request, emit func(Delta)) (*Response, error) {
	m.mu.Lock()
	m.Seen = append(m.Seen, req)
	var step Step
	switch {
	case m.Reply != nil:
		reply := m.Reply
		m.mu.Unlock()
		step = reply(req)
		m.mu.Lock()
	case len(m.Steps) > 0:
		step, m.Steps = m.Steps[0], m.Steps[1:]
	default:
		step = echo(req)
	}
	n := len(m.Seen)
	m.mu.Unlock()
	if step.Err != nil {
		return nil, step.Err
	}
	msg := Message{Role: "assistant"}
	stream := func(kind, text string) error {
		for _, w := range strings.SplitAfter(text, " ") {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.Delay):
			}
			emit(Delta{Kind: kind, Text: w})
		}
		return nil
	}
	if step.Thinking != "" {
		if err := stream(Thinking, step.Thinking); err != nil {
			return nil, err
		}
		msg.Blocks = append(msg.Blocks, Block{Type: Thinking, Text: step.Thinking})
	}
	if step.Text != "" {
		if err := stream(Text, step.Text); err != nil {
			return nil, err
		}
		msg.Blocks = append(msg.Blocks, Block{Type: Text, Text: step.Text})
	}
	stop := StopEnd
	for i, c := range step.Calls {
		c.Type = ToolCall
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d_%d", n, i)
		}
		if len(c.Input) == 0 {
			c.Input = json.RawMessage(`{}`)
		}
		emit(Delta{Kind: "tool", ID: c.ID, Name: c.Name, Text: string(c.Input)})
		msg.Blocks = append(msg.Blocks, c)
		stop = StopTool
	}
	usage := step.Usage
	if usage.Input == 0 {
		usage.Input = EstimateTokens(req)
		usage.Output = len(step.Text)/4 + 1
	}
	if step.Stop != "" {
		stop = step.Stop
	}
	return &Response{Message: msg, Usage: usage, Stop: stop}, nil
}

func echo(req Request) Step {
	if len(req.Messages) == 0 {
		return Step{Text: "Hello."}
	}
	if step, ok := demo(req); ok {
		return step
	}
	last := req.Messages[len(req.Messages)-1]
	for _, b := range last.Blocks {
		if b.Type == ToolResult {
			return Step{Text: "The tool returned:\n\n```\n" + b.Text + "\n```"}
		}
	}
	text := said(last)
	if rest, ok := strings.CutPrefix(text, "tool "); ok {
		name, args, _ := strings.Cut(rest, " ")
		if !json.Valid([]byte(args)) {
			args = "{}"
		}
		return Step{Thinking: "The user asked me to call " + name + ".", Calls: []Block{{Name: name, Input: json.RawMessage(args)}}}
	}
	return Step{Thinking: "Echoing the message back.", Text: "You said: " + text}
}

// said is the text of a user message without the time stamp Artus puts first.
func said(m Message) string {
	text := m.TextOf()
	if i := strings.LastIndex(text, "</time>"); i >= 0 {
		text = text[i+len("</time>"):]
	}
	return strings.TrimSpace(text)
}

// demo scripts a run of several steps, for trying the interface by hand. The
// message "demo" thinks, calls tools, says something in between, fails once
// and answers; "demo slow" runs a command that takes a while; "demo agents"
// starts two sub-agents that each do the slow run.
func demo(req Request) (Step, bool) {
	turn := 0 // replies already given to the message
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		results := false
		for _, b := range m.Blocks {
			results = results || b.Type == ToolResult
		}
		switch {
		case m.Role == "assistant":
			turn++
		case results:
		default:
			kind, ok := strings.CutPrefix(said(m), "demo")
			if !ok {
				return Step{}, false
			}
			script := demos[strings.TrimSpace(kind)]
			if turn >= len(script) {
				return Step{}, false
			}
			return script[turn], true
		}
	}
	return Step{}, false
}

func call(name, args string) Block { return Block{Name: name, Input: json.RawMessage(args)} }

const slowly = `{"command":"for i in 1 2 3 4 5 6; do echo step $i; sleep 1; done"}`

var demos = map[string][]Step{
	"": {
		{Thinking: "The user wants to see a run with several steps. I will look around the folder first, then list its notes, and only then decide what else is worth checking before I answer.",
			Calls: []Block{call("bash", `{"command":"echo Looking around; ls | head -5; sleep 1"}`), call("glob", `{"pattern":"*.md"}`)}},
		{Thinking: "Both calls worked. One more look before the answer.",
			Text:  "The folder looks as expected. Next I read a file that may not exist and search the notes.",
			Calls: []Block{call("read", `{"path":"does-not-exist.md"}`), call("grep", `{"pattern":"Artus","path":"."}`)}},
		{Calls: []Block{call("bash", `{"command":"for i in 1 2 3; do echo step $i; sleep 1; done"}`)}},
		{Thinking: "That is everything I set out to check.",
			Text: "Done. Here is what I found:\n\n- The folder holds the usual files.\n- `does-not-exist.md` is missing, as its name says.\n- The last command counted to three."},
	},
	"slow": {
		{Thinking: "A command that takes a while, so that its progress can be watched.", Calls: []Block{call("bash", slowly)}},
		{Text: "The slow command has finished."},
	},
	"agents": {
		{Thinking: "Two parts that do not depend on each other: one sub-agent each.",
			Calls: []Block{call("agent", `{"name":"Survey the notes","prompt":"demo slow"}`), call("agent", `{"name":"Check the tools","prompt":"demo slow"}`)}},
		{Text: "Both sub-agents have reported back."},
	},
}

// EstimateTokens roughly sizes a request (about four characters per token)
// for providers that do not report usage.
func EstimateTokens(req Request) int {
	n := len(req.System)
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			n += len(b.Text) + len(b.Input)
			if b.Type == Image || b.Type == File {
				n += 6000
			}
		}
	}
	return n / 4
}
