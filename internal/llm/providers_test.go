package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeAPI serves canned server-sent events and records what it was sent.
type fakeAPI struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	header http.Header
	reply  func(n int, body map[string]any) (status int, payload string)
}

func newFakeAPI(t *testing.T, reply func(n int, body map[string]any) (int, string)) *fakeAPI {
	f := &fakeAPI{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r) // no model metadata: adapters must cope
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.header = r.Header.Clone()
		n := len(f.bodies)
		f.mu.Unlock()
		status, payload := f.reply(n, body)
		if status != 200 {
			w.WriteHeader(status)
		}
		io.WriteString(w, payload)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) body(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

func staticKey(context.Context) (string, error) { return "test-key", nil }

// sse joins events into a stream; each argument is one JSON payload.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

func collect(deltas *[]Delta) func(Delta) { return func(d Delta) { *deltas = append(*deltas, d) } }

func joined(deltas []Delta, kind string) string {
	var b strings.Builder
	for _, d := range deltas {
		if d.Kind == kind {
			b.WriteString(d.Name + d.Text)
		}
	}
	return b.String()
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var noteTool = Tool{Name: "note", Description: "Save a note", Schema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)}

func TestAnthropicStreamAndReplay(t *testing.T) {
	stream := sse(
		`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":90,"cache_creation_input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think."}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG123"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"ping"}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Saving "}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"it."}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"note","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"text\": \"mi"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"lk\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"some_future_event","anything":true}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
		`{"type":"message_stop"}`,
	)
	api := newFakeAPI(t, func(n int, body map[string]any) (int, string) {
		if n == 1 { // the endpoint does not know one optional field: the adapter must drop it and retry
			return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"tools.0.eager_input_streaming: Extra inputs are not permitted"}}`
		}
		return 200, stream
	})
	a := NewAnthropic(staticKey)
	a.Base = api.URL
	req := Request{ID: "anthropic/claude-test", Model: "claude-test", Effort: "high", System: "Be brief.", Tools: []Tool{noteTool}, WebSearch: true,
		Messages: []Message{UserText("remember milk")}}
	var deltas []Delta
	resp, err := a.Stream(context.Background(), req, collect(&deltas))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stop != StopTool || len(resp.Message.Blocks) != 3 {
		t.Fatalf("stop=%s blocks=%+v", resp.Stop, resp.Message.Blocks)
	}
	th, tx, call := resp.Message.Blocks[0], resp.Message.Blocks[1], resp.Message.Blocks[2]
	if th.Type != Thinking || th.Text != "Let me think." || !strings.Contains(string(th.Raw), `"signature":"SIG123"`) {
		t.Errorf("thinking block: %+v raw=%s", th, th.Raw)
	}
	if tx.Text != "Saving it." || call.ID != "toolu_1" || string(call.Input) != `{"text": "milk"}` {
		t.Errorf("text %q, call %+v", tx.Text, call)
	}
	if resp.Usage != (Usage{Input: 105, Output: 42, CacheRead: 90, CacheWrite: 5}) {
		t.Errorf("usage: %+v", resp.Usage)
	}
	if joined(deltas, Thinking) != "Let me think." || joined(deltas, Text) != "Saving it." || joined(deltas, "tool") != `note{"text": "milk"}` {
		t.Errorf("deltas: %+v", deltas)
	}

	sent := api.body(1)
	if api.header.Get("x-api-key") != "test-key" || api.header.Get("anthropic-version") != anthropicVersion || !strings.Contains(api.header.Get("anthropic-beta"), "thinking-binding-controls") {
		t.Errorf("headers: %v", api.header)
	}
	got := jsonOf(sent)
	for _, want := range []string{`"cache_control":{"type":"ephemeral"}`, `"type":"adaptive"`, `"display":"summarized"`, `"prefix_mismatch_behavior":"drop_block"`,
		`"output_config":{"effort":"high"}`, `"web_search_20260209"`, `"stream":true`, `"input_schema"`} {
		if !strings.Contains(got, want) {
			t.Errorf("request lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "eager_input_streaming") {
		t.Error("the rejected field was sent again")
	}
	if strings.Contains(jsonOf(api.body(0)), "eager_input_streaming") == false {
		t.Error("the first attempt should have tried the optional field")
	}
	if strings.Contains(got, "fallbacks") {
		t.Error("fallbacks are only for models that have them")
	}

	// Replay: this model's own turn goes back byte for byte; another model's turn is reduced.
	resp.Message.Model = req.ID
	result := Message{Role: "user", Blocks: []Block{{Type: Text, Text: "also eggs"}, {Type: ToolResult, ID: "toolu_1", Text: "saved"}}}
	req.Messages = append(req.Messages, resp.Message, result)
	if _, err := a.Stream(context.Background(), req, func(Delta) {}); err != nil {
		t.Fatal(err)
	}
	msgs := api.body(2)["messages"].([]any)
	assistant := jsonOf(msgs[1])
	if !strings.Contains(assistant, `"signature":"SIG123"`) || !strings.Contains(assistant, `"thinking":"Let me think."`) || !strings.Contains(assistant, `"input":{"text":"milk"}`) {
		t.Errorf("own turn was not replayed as received: %s", assistant)
	}
	user := msgs[2].(map[string]any)["content"].([]any)
	if first := user[0].(map[string]any); first["type"] != "tool_result" || first["tool_use_id"] != "toolu_1" {
		t.Errorf("tool results must lead the user turn: %s", jsonOf(user))
	}
	req.Messages[1].Model = "openai/gpt-x"
	a.Stream(context.Background(), req, func(Delta) {})
	foreign := jsonOf(api.body(3)["messages"].([]any)[1])
	if strings.Contains(foreign, "thinking") || !strings.Contains(foreign, `"tool_use"`) || !strings.Contains(foreign, "Saving it.") {
		t.Errorf("a turn from another model should keep text and calls only: %s", foreign)
	}
}

func TestAnthropicFallbacksAndErrors(t *testing.T) {
	api := newFakeAPI(t, func(n int, body map[string]any) (int, string) {
		switch n {
		case 1:
			return 200, sse(`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
				`{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"category":"cyber","explanation":"declined"}},"usage":{"output_tokens":1}}`)
		case 2:
			return 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
		default:
			return 200, sse(`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
				`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded mid-stream"}}`)
		}
	})
	a := NewAnthropic(staticKey)
	a.Base = api.URL
	req := Request{ID: "anthropic/claude-opus-5-5", Model: "claude-opus-5-5", Effort: "medium", Messages: []Message{UserText("hi")}}
	resp, err := a.Stream(context.Background(), req, func(Delta) {})
	if err != nil || resp.Stop != StopRefusal || !strings.Contains(resp.Detail, "cyber") {
		t.Fatalf("refusal: %+v %v", resp, err)
	}
	if sent := jsonOf(api.body(0)); !strings.Contains(sent, `"fallbacks":"default"`) || !strings.Contains(api.header.Get("anthropic-beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("a model with refusal fallbacks should opt in: %s", sent)
	}
	for i, wantMsg := range []string{"Overloaded", "Overloaded mid-stream"} {
		_, err := a.Stream(context.Background(), req, func(Delta) {})
		e, ok := err.(*Error)
		if !ok || e.Status != 529 || e.Message != wantMsg || !e.Retryable() {
			t.Errorf("error %d: %#v", i, err)
		}
	}
	noKey := NewAnthropic(func(context.Context) (string, error) { return "", fmt.Errorf("no ANTHROPIC_API_KEY") })
	if _, err := noKey.Stream(context.Background(), req, func(Delta) {}); err == nil || err.(*Error).Retryable() {
		t.Errorf("a missing key must fail without retrying: %v", err)
	}
}

func TestOpenAIResponses(t *testing.T) {
	stream := sse(
		`{"type":"response.created"}`,
		`{"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"Working it out."}`,
		`{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Working it out."}],"encrypted_content":"ENC"}}`,
		`{"type":"response.output_text.delta","delta":"On "}`,
		`{"type":"response.output_text.delta","delta":"it."}`,
		`{"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"On it."}]}}`,
		`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","name":"note","call_id":"call_9"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"text\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"milk\"}"}`,
		`{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","name":"note","call_id":"call_9","arguments":"{\"text\":\"milk\"}"}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":200,"output_tokens":30,"input_tokens_details":{"cached_tokens":150}}}}`,
	)
	api := newFakeAPI(t, func(n int, body map[string]any) (int, string) {
		if n == 1 {
			return 400, `{"error":{"message":"Unsupported value: 'xhigh' is not supported with this model.","type":"invalid_request_error"}}`
		}
		return 200, stream
	})
	o := NewOpenAI(staticKey)
	o.Base = api.URL
	img := filepath.Join(t.TempDir(), "p.png")
	os.WriteFile(img, []byte("PNGDATA"), 0o600)
	req := Request{ID: "openai/gpt-test", Model: "gpt-test", Effort: "max", System: "Be brief.", Cache: "chat-1", Tools: []Tool{noteTool}, WebSearch: true,
		Messages: []Message{{Role: "user", Blocks: []Block{{Type: Text, Text: "remember milk"}, {Type: Image, Path: img, MediaType: "image/png"}, {Type: Image, Path: img + ".gone", MediaType: "image/png"}}}}}
	var deltas []Delta
	resp, err := o.Stream(context.Background(), req, collect(&deltas))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stop != StopTool || len(resp.Message.Blocks) != 3 || resp.Message.Blocks[0].Text != "Working it out." || resp.Message.TextOf() != "On it." {
		t.Fatalf("response: stop=%s %+v", resp.Stop, resp.Message.Blocks)
	}
	if c := resp.Message.Calls()[0]; c.ID != "call_9" || string(c.Input) != `{"text":"milk"}` {
		t.Errorf("call: %+v", c)
	}
	if resp.Usage != (Usage{Input: 200, Output: 30, CacheRead: 150}) {
		t.Errorf("usage: %+v", resp.Usage)
	}
	if joined(deltas, Thinking) != "Working it out." || joined(deltas, Text) != "On it." || joined(deltas, "tool") != `note{"text":"milk"}` {
		t.Errorf("deltas: %+v", deltas)
	}
	first, second := jsonOf(api.body(0)), jsonOf(api.body(1))
	if !strings.Contains(first, `"effort":"xhigh"`) || !strings.Contains(second, `"effort":"high"`) {
		t.Errorf("max should try xhigh, then settle on high when the model rejects it:\n%s\n%s", first, second)
	}
	for _, want := range []string{`"store":false`, `"instructions":"Be brief."`, `"reasoning.encrypted_content"`, `"prompt_cache_key":"chat-1"`, `"summary":"auto"`,
		`"type":"web_search"`, `"image_url":"data:image/png;base64,UE5HREFUQQ=="`, `no longer on disk`, `"type":"function"`} {
		if !strings.Contains(second, want) {
			t.Errorf("request lacks %s:\n%s", want, second)
		}
	}

	resp.Message.Model = req.ID
	req.Messages = append(req.Messages, resp.Message, Message{Role: "user", Blocks: []Block{{Type: ToolResult, ID: "call_9", Text: "saved"}}})
	o.Stream(context.Background(), req, func(Delta) {})
	items := api.body(2)["input"].([]any)
	var kinds []string
	for _, it := range items[1:] {
		m := it.(map[string]any)
		if kind, ok := m["type"].(string); ok {
			kinds = append(kinds, kind)
		} else {
			kinds = append(kinds, m["role"].(string))
		}
	}
	if strings.Join(kinds, ",") != "reasoning,assistant,function_call,function_call_output" || !strings.Contains(jsonOf(items[1]), `"encrypted_content":"ENC"`) {
		t.Errorf("replayed items: %v\n%s", kinds, jsonOf(items))
	}
}

func TestOpenRouterChat(t *testing.T) {
	stream := ": OPENROUTER PROCESSING\n\n" + sse(
		`{"choices":[{"delta":{"role":"assistant","content":"","reasoning":"Hmm, ","reasoning_details":[{"type":"reasoning.text","index":0,"text":"Hmm, ","format":"anthropic-claude-v1"}]}}]}`,
		`{"choices":[{"delta":{"reasoning":"milk.","reasoning_details":[{"type":"reasoning.text","index":0,"text":"milk.","signature":"SIG"}]}}]}`,
		`{"choices":[{"delta":{"content":"Noted"}}]}`,
		`{"choices":[{"delta":{"content":".","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"note","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"text\":\"mi"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"lk\"}"}},{"index":1,"id":"call_b","function":{"name":"note","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"content":""},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":40,"cost":0.0012,"prompt_tokens_details":{"cached_tokens":250,"cache_write_tokens":20}}}`,
		`[DONE]`,
	)
	api := newFakeAPI(t, func(n int, body map[string]any) (int, string) {
		if n == 3 {
			return 200, sse(`{"choices":[{"delta":{"content":"par"}}]}`, `{"error":{"code":429,"message":"Rate limited upstream"},"choices":[]}`)
		}
		return 200, stream
	})
	o := NewOpenRouter(staticKey)
	o.Base = api.URL
	req := Request{ID: "openrouter/anthropic/claude-x", Model: "anthropic/claude-x", Effort: "xhigh", System: "Be brief.", Tools: []Tool{noteTool}, WebSearch: true,
		Messages: []Message{UserText("remember milk")}}
	var deltas []Delta
	resp, err := o.Stream(context.Background(), req, collect(&deltas))
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.Calls()
	if resp.Stop != StopTool || resp.Message.TextOf() != "Noted." || len(calls) != 2 || string(calls[0].Input) != `{"text":"milk"}` || calls[1].ID != "call_b" {
		t.Fatalf("response: stop=%s %+v", resp.Stop, resp.Message.Blocks)
	}
	th := resp.Message.Blocks[0]
	if th.Type != Thinking || th.Text != "Hmm, milk." || !strings.Contains(string(th.Raw), `"text":"Hmm, milk."`) || !strings.Contains(string(th.Raw), `"signature":"SIG"`) {
		t.Errorf("reasoning fragments were not merged: %+v raw=%s", th, th.Raw)
	}
	if resp.Usage != (Usage{Input: 300, Output: 40, CacheRead: 250, CacheWrite: 20, Cost: 0.0012}) {
		t.Errorf("usage: %+v", resp.Usage)
	}
	sent := jsonOf(api.body(0))
	for _, want := range []string{`"reasoning":{"effort":"xhigh"}`, `"openrouter:web_search"`, `"usage":{"include":true}`, `"role":"system"`, `"cache_control"`} {
		if !strings.Contains(sent, want) {
			t.Errorf("request lacks %s:\n%s", want, sent)
		}
	}
	if api.header.Get("Authorization") != "Bearer test-key" || api.header.Get("HTTP-Referer") != "" || api.header.Get("X-Title") != "" {
		t.Errorf("headers should carry the key and nothing identifying: %v", api.header)
	}

	resp.Message.Model = req.ID
	req.Messages = append(req.Messages, resp.Message, Message{Role: "user", Blocks: []Block{
		{Type: ToolResult, ID: "call_a", Text: "saved"}, {Type: ToolResult, ID: "call_b", Text: ""}}})
	o.Stream(context.Background(), req, func(Delta) {})
	msgs := api.body(1)["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("want system, user, assistant, tool, tool; got %s", jsonOf(msgs))
	}
	assistant := jsonOf(msgs[2])
	if !strings.Contains(assistant, `"reasoning_details":[{`) || !strings.Contains(assistant, `"signature":"SIG"`) || !strings.Contains(assistant, `"tool_calls"`) {
		t.Errorf("the model's own reasoning must be sent back with its turn: %s", assistant)
	}
	if last := jsonOf(msgs[4]); !strings.Contains(last, `"tool_call_id":"call_b"`) || !strings.Contains(last, "(no output)") {
		t.Errorf("tool message: %s", last)
	}

	_, err = o.Stream(context.Background(), req, func(Delta) {})
	if e, ok := err.(*Error); !ok || e.Status != 429 || !strings.Contains(e.Message, "Rate limited upstream") {
		t.Errorf("an error inside the stream must surface with its status: %#v", err)
	}
}

func TestNormalizeAndEffort(t *testing.T) {
	msgs := normalize([]Message{
		UserText("a"),
		{Role: "user", Blocks: []Block{{Type: Text, Text: "b"}, {Type: ToolResult, ID: "1", Text: "r"}}},
		{Role: "assistant", Blocks: []Block{{Type: Text, Text: "c"}}},
		{Role: "user"},
		{Role: "user", Blocks: []Block{{Type: Text, Text: "d"}}},
	})
	if len(msgs) != 3 || msgs[0].Blocks[0].Type != ToolResult || len(msgs[0].Blocks) != 3 || msgs[2].TextOf() != "d" {
		t.Errorf("normalize: %+v", msgs)
	}
	for _, c := range []struct{ effort, want string }{{"max", "xhigh"}, {"xhigh", "xhigh"}, {"low", "low"}, {"", "medium"}, {"bogus", "medium"}} {
		if got := ClampEffort(c.effort, "low", "medium", "high", "xhigh"); got != c.want {
			t.Errorf("ClampEffort(%q) = %q, want %q", c.effort, got, c.want)
		}
	}
	if got := ClampEffort("max", "low", "medium", "high"); got != "high" {
		t.Errorf("max clamps to the highest supported: %q", got)
	}
	if string(arguments(`{"a":1`)) != `"{\"a\":1"` || string(arguments("")) != "{}" || string(arguments(` {"a":1} `)) != `{"a":1}` {
		t.Error("arguments: truncated JSON must become a string, empty an object")
	}
}
