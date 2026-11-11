package llm

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests call real APIs and cost a fraction of a cent. They run only
// when ARTUS_LIVE is set and the matching key is in the environment:
//
//	ARTUS_LIVE=1 OPENROUTER_API_KEY=… go test ./internal/llm -run Live -v
//
// One OpenRouter key exercises all three wire formats, because OpenRouter
// also serves Messages- and Responses-compatible endpoints.
func liveKey(t *testing.T, name string) KeyFunc {
	t.Helper()
	key := os.Getenv(name)
	if os.Getenv("ARTUS_LIVE") == "" || key == "" {
		t.Skip("set ARTUS_LIVE=1 and " + name + " to run live tests")
	}
	return func(context.Context) (string, error) { return key, nil }
}

var addTool = Tool{
	Name:        "add",
	Description: "Add two integers and return the sum.",
	Schema:      json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
}

// toolLoop runs a two-request exchange: the model must call the tool, then
// use its result. The second request replays the first reply, which is where
// mistakes in carrying reasoning forward show up.
func toolLoop(t *testing.T, p Provider, id, model, effort string) {
	t.Helper()
	question := "What is 1234 plus 4321? Use the tool, then answer in one short sentence."
	if effort == "high" { // make the model reason first, so reasoning has to survive the replay
		question = "A farmer has 1234 sheep and buys 4321 more, but first work out step by step whether 2^31 is larger than 3^19. Then use the tool to add the sheep, and answer both in two short sentences. The total must appear as a plain number."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req := Request{
		ID: id, Model: model, Effort: effort, Cache: "live-test",
		System:   "You are a careful assistant. Always use the add tool for arithmetic; never compute sums yourself.",
		Tools:    []Tool{addTool},
		Messages: []Message{UserText(question)},
	}
	var streamed struct{ text, thinking, tool strings.Builder }
	emit := func(d Delta) {
		switch d.Kind {
		case Text:
			streamed.text.WriteString(d.Text)
		case Thinking:
			streamed.thinking.WriteString(d.Text)
		case "tool":
			streamed.tool.WriteString(d.Name + d.Text)
		}
	}
	first, err := p.Stream(ctx, req, emit)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	calls := first.Message.Calls()
	if first.Stop != StopTool || len(calls) != 1 || calls[0].Name != "add" {
		t.Fatalf("expected one add call, got stop=%s blocks=%+v", first.Stop, first.Message.Blocks)
	}
	var args struct{ A, B int }
	if err := json.Unmarshal(calls[0].Input, &args); err != nil || args.A+args.B != 5555 {
		t.Fatalf("tool arguments: %s (%v)", calls[0].Input, err)
	}
	if !strings.Contains(streamed.tool.String(), "add") {
		t.Errorf("the tool call was not streamed: %q", streamed.tool.String())
	}
	if first.Usage.Input == 0 || first.Usage.Output == 0 {
		t.Errorf("usage missing: %+v", first.Usage)
	}
	t.Logf("first: usage=%+v thinking=%d chars blocks=%d", first.Usage, streamed.thinking.Len(), len(first.Message.Blocks))

	first.Message.Model = id
	req.Messages = append(req.Messages, first.Message, Message{Role: "user", Blocks: []Block{
		{Type: ToolResult, ID: calls[0].ID, Name: "add", Text: "5555"},
	}})
	streamed.text.Reset()
	second, err := p.Stream(ctx, req, emit)
	if err != nil {
		t.Fatalf("second request (replaying the first reply): %v", err)
	}
	if second.Stop != StopEnd || !strings.Contains(strings.ReplaceAll(second.Message.TextOf(), ",", ""), "5555") {
		t.Fatalf("expected the sum in the final answer, got stop=%s text=%q", second.Stop, second.Message.TextOf())
	}
	if streamed.text.String() != second.Message.TextOf() {
		t.Errorf("streamed text %q differs from final text %q", streamed.text.String(), second.Message.TextOf())
	}
	t.Logf("second: usage=%+v text=%q", second.Usage, second.Message.TextOf())

	// A turn written by another model must be accepted too (model switch mid-chat).
	foreign := req
	foreign.Messages = append([]Message(nil), req.Messages...)
	other := first.Message
	other.Model = "someone/else"
	foreign.Messages[1] = other
	if _, err := p.Stream(ctx, foreign, func(Delta) {}); err != nil {
		t.Fatalf("history from another model was rejected: %v", err)
	}
}

func TestLiveOpenRouter(t *testing.T) {
	p := NewOpenRouter(liveKey(t, "OPENROUTER_API_KEY"))
	models, err := p.Models(context.Background())
	if err != nil || len(models) < 20 {
		t.Fatalf("models: %d %v", len(models), err)
	}
	for _, m := range models {
		if m.ID == "anthropic/claude-haiku-5.5" && (m.Window == 0 || !m.Vision || !m.Reasoning || m.PriceIn <= 0) {
			t.Errorf("model details missing: %+v", m)
		}
	}
	for _, model := range []string{"anthropic/claude-haiku-5.5", "openai/gpt-5.6-luna", "google/gemini-3.5-flash-lite"} {
		t.Run(model, func(t *testing.T) { toolLoop(t, p, "openrouter/"+model, model, "low") })
	}
}

func TestLiveAnthropicFormat(t *testing.T) {
	p := NewAnthropic(liveKey(t, "OPENROUTER_API_KEY"))
	p.Base, p.Bearer = "https://openrouter.ai/api", true
	for _, effort := range []string{"low", "high"} {
		t.Run(effort, func(t *testing.T) {
			toolLoop(t, p, "anthropic/anthropic/claude-haiku-5.5", "anthropic/claude-haiku-5.5", effort)
		})
	}
}

func TestLiveOpenAIFormat(t *testing.T) {
	p := NewOpenAI(liveKey(t, "OPENROUTER_API_KEY"))
	p.Base = "https://openrouter.ai/api/v1"
	for _, effort := range []string{"low", "high"} {
		t.Run(effort, func(t *testing.T) {
			toolLoop(t, p, "openai/openai/gpt-5.6-luna", "openai/gpt-5.6-luna", effort)
		})
	}
}

func TestLiveOpenRouterReasoning(t *testing.T) {
	p := NewOpenRouter(liveKey(t, "OPENROUTER_API_KEY"))
	for _, model := range []string{"anthropic/claude-haiku-5.5", "openai/gpt-5.6-luna", "google/gemini-3.5-flash"} {
		t.Run(model, func(t *testing.T) { toolLoop(t, p, "openrouter/"+model, model, "high") })
	}
}

func TestLiveAnthropic(t *testing.T) {
	p := NewAnthropic(liveKey(t, "ANTHROPIC_API_KEY"))
	toolLoop(t, p, "anthropic/claude-haiku-5-5", "claude-haiku-5-5", "low")
}

func TestLiveOpenAI(t *testing.T) {
	p := NewOpenAI(liveKey(t, "OPENAI_API_KEY"))
	toolLoop(t, p, "openai/gpt-5.6-luna", "gpt-5.6-luna", "low")
}
