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
// result, which is enough to exercise the whole loop by hand.
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
	last := req.Messages[len(req.Messages)-1]
	for _, b := range last.Blocks {
		if b.Type == ToolResult {
			return Step{Text: "The tool returned:\n\n```\n" + b.Text + "\n```"}
		}
	}
	text := last.TextOf()
	if i := strings.LastIndex(text, "</time>"); i >= 0 {
		text = strings.TrimSpace(text[i+len("</time>"):])
	}
	if rest, ok := strings.CutPrefix(text, "tool "); ok {
		name, args, _ := strings.Cut(rest, " ")
		if !json.Valid([]byte(args)) {
			args = "{}"
		}
		return Step{Thinking: "The user asked me to call " + name + ".", Calls: []Block{{Name: name, Input: json.RawMessage(args)}}}
	}
	return Step{Thinking: "Echoing the message back.", Text: "You said: " + text}
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
