package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Anthropic talks to the Claude Messages API.
type Anthropic struct {
	key KeyFunc
	// Base and Bearer exist for Messages-compatible endpoints run by others:
	// Base replaces https://api.anthropic.com and Bearer sends the key as an
	// Authorization header instead of x-api-key.
	Base   string
	Bearer bool

	quirks quirks
	mu     sync.Mutex
	caps   map[string]anthropicCaps
}

// NewAnthropic returns the provider for api.anthropic.com.
func NewAnthropic(key KeyFunc) *Anthropic {
	return &Anthropic{key: key, Base: "https://api.anthropic.com", caps: map[string]anthropicCaps{}}
}

const anthropicVersion = "2026-10-21"

// anthropicCaps is what a model supports, read from the Models API so new
// models work without a release.
type anthropicCaps struct {
	Window, MaxOutput int
	Vision            bool
	Adaptive, Budget  bool     // thinking modes: adaptive, or a fixed token budget
	Efforts           []string // supported effort levels
}

type anthropicModel struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	MaxInput     int    `json:"max_input_tokens"`
	MaxTokens    int    `json:"max_tokens"`
	Capabilities struct {
		ImageInput struct{ Supported bool } `json:"image_input"`
		Thinking   struct {
			Types struct {
				Adaptive struct{ Supported bool } `json:"adaptive"`
				Enabled  struct{ Supported bool } `json:"enabled"`
			} `json:"types"`
		} `json:"thinking"`
		Effort map[string]json.RawMessage `json:"effort"`
	} `json:"capabilities"`
}

func (m anthropicModel) caps() anthropicCaps {
	c := anthropicCaps{Window: m.MaxInput, MaxOutput: m.MaxTokens, Vision: m.Capabilities.ImageInput.Supported,
		Adaptive: m.Capabilities.Thinking.Types.Adaptive.Supported, Budget: m.Capabilities.Thinking.Types.Enabled.Supported}
	for _, level := range Efforts {
		var leaf struct{ Supported bool }
		if json.Unmarshal(m.Capabilities.Effort[level], &leaf) == nil && leaf.Supported {
			c.Efforts = append(c.Efforts, level)
		}
	}
	return c
}

func (a *Anthropic) headers(ctx context.Context, betas ...string) (map[string]string, error) {
	key, err := a.key(ctx)
	if err != nil {
		return nil, &Error{Status: 401, Message: err.Error()}
	}
	h := map[string]string{"anthropic-version": anthropicVersion}
	if a.Bearer {
		h["Authorization"] = "Bearer " + key
	} else {
		h["x-api-key"] = key
	}
	if len(betas) > 0 {
		h["anthropic-beta"] = strings.Join(betas, ",")
	}
	return h, nil
}

// Models implements Provider.
func (a *Anthropic) Models(ctx context.Context) ([]Model, error) {
	h, err := a.headers(ctx)
	if err != nil {
		return nil, err
	}
	var out []Model
	after := ""
	for page := 0; page < 20; page++ {
		var list struct {
			Data    []anthropicModel `json:"data"`
			HasMore bool             `json:"has_more"`
			LastID  string           `json:"last_id"`
		}
		url := a.Base + "/v1/models?limit=1000"
		if after != "" {
			url += "&after_id=" + after
		}
		if err := getJSON(ctx, url, h, &list); err != nil {
			return nil, err
		}
		a.mu.Lock()
		for _, m := range list.Data {
			c := m.caps()
			a.caps[m.ID] = c
			out = append(out, Model{ID: m.ID, Name: m.DisplayName, Window: c.Window, Vision: c.Vision, Reasoning: len(c.Efforts) > 0 || c.Adaptive || c.Budget})
		}
		a.mu.Unlock()
		if !list.HasMore || list.LastID == "" {
			break
		}
		after = list.LastID
	}
	return out, nil
}

// capabilities returns what a model supports, asking the API once. If the
// lookup fails the model is assumed to be a current one.
func (a *Anthropic) capabilities(ctx context.Context, h map[string]string, model string) anthropicCaps {
	a.mu.Lock()
	c, ok := a.caps[model]
	a.mu.Unlock()
	if ok {
		return c
	}
	var m anthropicModel
	if err := getJSON(ctx, a.Base+"/v1/models/"+model, h, &m); err == nil && m.ID != "" {
		c = m.caps()
	} else {
		c = anthropicCaps{MaxOutput: 64000, Vision: true, Adaptive: true, Efforts: Efforts}
	}
	a.mu.Lock()
	a.caps[model] = c
	a.mu.Unlock()
	return c
}

// Optional request features, most specific first.
var anthropicFeatures = []feature{
	{"fallbacks", []string{"fallbacks", "server-side-fallback"}},
	{"binding", []string{"block_binding", "thinking-binding"}},
	{"eager", []string{"eager_input_streaming"}},
	{"search", []string{"web_search"}},
	{"cache_control", []string{"cache_control"}},
	{"effort", []string{"output_config", "effort"}},
	{"thinking", []string{"thinking"}},
}

// Models for which a safety classifier may decline a request, and the API
// can re-serve it on a fallback model inside the same call.
var fallbackModels = []string{"claude-fable-5-2", "claude-fable-5-1", "claude-mythos-5-2", "claude-mythos-5-1", "claude-opus-5", "claude-sonnet-5-5"}

// Stream implements Provider.
func (a *Anthropic) Stream(ctx context.Context, req Request, emit func(Delta)) (*Response, error) {
	total := &Response{Message: Message{Role: "assistant"}}
	messages := a.messages(req)
	for round := 0; round < 12; round++ {
		resp, raw, err := a.once(ctx, req, messages, emit)
		if err != nil {
			return nil, err
		}
		total.Message.Blocks = append(total.Message.Blocks, resp.Message.Blocks...)
		total.Usage.Input = resp.Usage.Input // the latest request holds the whole context
		total.Usage.Output += resp.Usage.Output
		total.Usage.CacheRead, total.Usage.CacheWrite = resp.Usage.CacheRead, resp.Usage.CacheWrite
		total.Stop, total.Detail = resp.Stop, resp.Detail
		if resp.Stop != "pause" {
			return total, nil
		}
		// A long server-side tool run paused the turn: hand the partial turn back to continue it.
		messages = append(messages, map[string]any{"role": "assistant", "content": raw})
	}
	total.Stop = StopEnd
	return total, nil
}

// once sends one request, retrying without any optional feature the endpoint rejects.
func (a *Anthropic) once(ctx context.Context, req Request, messages []any, emit func(Delta)) (*Response, []any, error) {
	for attempt := 0; ; attempt++ {
		resp, raw, err := a.send(ctx, req, messages, emit)
		if feature := rejected(err, anthropicFeatures); feature != "" && attempt < len(anthropicFeatures) && !a.quirks.disabled(req.Model, feature) {
			a.quirks.disable(req.Model, feature)
			continue
		}
		return resp, raw, err
	}
}

func (a *Anthropic) send(ctx context.Context, req Request, messages []any, emit func(Delta)) (*Response, []any, error) {
	var betas []string
	on := func(feature string) bool { return !a.quirks.disabled(req.Model, feature) }
	probe, err := a.headers(ctx)
	if err != nil {
		return nil, nil, err
	}
	caps := a.capabilities(ctx, probe, req.Model)

	maxTokens := 64000
	if caps.MaxOutput > 0 && caps.MaxOutput < maxTokens {
		maxTokens = caps.MaxOutput
	}
	body := map[string]any{"model": req.Model, "max_tokens": maxTokens, "stream": true, "messages": messages}
	if req.System != "" {
		block := map[string]any{"type": "text", "text": req.System}
		if on("cache_control") {
			// One breakpoint on the frozen prompt, and automatic caching of the growing conversation.
			block["cache_control"] = map[string]string{"type": "ephemeral"}
			body["cache_control"] = map[string]string{"type": "ephemeral"}
		}
		body["system"] = []any{block}
	}
	var tools []any
	for _, t := range req.Tools {
		tool := map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Schema}
		if on("eager") {
			tool["eager_input_streaming"] = true // large arguments stream as they are written
		}
		tools = append(tools, tool)
	}
	if req.WebSearch && on("search") {
		tools = append(tools, map[string]any{"type": "web_search_20260209", "name": "web_search"})
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if on("thinking") {
		switch {
		case caps.Adaptive:
			thinking := map[string]any{"type": "adaptive", "display": "summarized"}
			if on("binding") {
				// Reasoning is signed against the history that produced it. If the history
				// ever differs (a model switch, a fork), drop the affected reasoning
				// instead of failing the request.
				thinking["block_binding"] = map[string]string{"prefix_mismatch_behavior": "drop_block"}
				betas = append(betas, "thinking-binding-controls-2026-08-01")
			}
			body["thinking"] = thinking
		case caps.Budget:
			budget := map[string]int{"low": 2000, "medium": 8000, "high": 16000, "xhigh": 32000, "max": 48000}[req.Effort]
			if budget == 0 {
				budget = 8000
			}
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": min(budget, maxTokens-1024)}
		}
	}
	if len(caps.Efforts) > 0 && on("effort") {
		body["output_config"] = map[string]string{"effort": ClampEffort(req.Effort, caps.Efforts...)}
	}
	if on("fallbacks") {
		for _, prefix := range fallbackModels {
			if strings.HasPrefix(req.Model, prefix) {
				body["fallbacks"] = "default"
				betas = append(betas, "server-side-fallback-2026-07-01")
				break
			}
		}
	}
	headers, err := a.headers(ctx, betas...)
	if err != nil {
		return nil, nil, err
	}

	var blocks []map[string]any
	partial := map[int]*strings.Builder{}
	resp := &Response{Message: Message{Role: "assistant"}, Stop: StopEnd}
	usage := func(raw json.RawMessage) {
		var u struct {
			Input      *int `json:"input_tokens"`
			Output     *int `json:"output_tokens"`
			CacheRead  *int `json:"cache_read_input_tokens"`
			CacheWrite *int `json:"cache_creation_input_tokens"`
		}
		if json.Unmarshal(raw, &u) != nil {
			return
		}
		if u.CacheRead != nil {
			resp.Usage.CacheRead = *u.CacheRead
		}
		if u.CacheWrite != nil {
			resp.Usage.CacheWrite = *u.CacheWrite
		}
		if u.Input != nil {
			resp.Usage.Input = *u.Input + resp.Usage.CacheRead + resp.Usage.CacheWrite
		}
		if u.Output != nil {
			resp.Usage.Output = *u.Output
		}
	}
	err = postStream(ctx, a.Base+"/v1/messages", headers, body, func(_ string, data []byte) error {
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
			Block map[string]any `json:"content_block"`
			Delta struct {
				Type        string          `json:"type"`
				Text        string          `json:"text"`
				Thinking    string          `json:"thinking"`
				Signature   string          `json:"signature"`
				PartialJSON string          `json:"partial_json"`
				Citation    json.RawMessage `json:"citation"`
				StopReason  string          `json:"stop_reason"`
				StopDetails struct {
					Category, Explanation string
				} `json:"stop_details"`
			} `json:"delta"`
			Usage json.RawMessage `json:"usage"`
			Error struct{ Type, Message string }
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil // tolerate event types this client does not know
		}
		switch ev.Type {
		case "message_start":
			usage(ev.Message.Usage)
		case "content_block_start":
			for len(blocks) <= ev.Index {
				blocks = append(blocks, nil)
			}
			blocks[ev.Index] = ev.Block
			if kind, _ := ev.Block["type"].(string); kind == "tool_use" || kind == "server_tool_use" {
				partial[ev.Index] = &strings.Builder{}
				id, _ := ev.Block["id"].(string)
				name, _ := ev.Block["name"].(string)
				emit(Delta{Kind: "tool", ID: id, Name: name})
			}
		case "content_block_delta":
			if ev.Index >= len(blocks) || blocks[ev.Index] == nil {
				return nil
			}
			b := blocks[ev.Index]
			switch ev.Delta.Type {
			case "text_delta":
				b["text"] = str(b["text"]) + ev.Delta.Text
				emit(Delta{Kind: Text, Text: ev.Delta.Text})
			case "thinking_delta":
				b["thinking"] = str(b["thinking"]) + ev.Delta.Thinking
				emit(Delta{Kind: Thinking, Text: ev.Delta.Thinking})
			case "signature_delta":
				b["signature"] = str(b["signature"]) + ev.Delta.Signature
			case "input_json_delta":
				if p := partial[ev.Index]; p != nil {
					p.WriteString(ev.Delta.PartialJSON)
					emit(Delta{Kind: "tool", ID: str(b["id"]), Text: ev.Delta.PartialJSON})
				}
			case "citations_delta":
				list, _ := b["citations"].([]any)
				b["citations"] = append(list, ev.Delta.Citation)
			}
		case "content_block_stop":
			if p := partial[ev.Index]; p != nil && ev.Index < len(blocks) {
				blocks[ev.Index]["input"] = json.RawMessage(orEmptyObject(p.String()))
			}
		case "message_delta":
			usage(ev.Usage)
			switch ev.Delta.StopReason {
			case "tool_use":
				resp.Stop = StopTool
			case "max_tokens", "model_context_window_exceeded":
				resp.Stop = StopLength
			case "refusal":
				resp.Stop = StopRefusal
				resp.Detail = strings.TrimSpace(ev.Delta.StopDetails.Category + " " + ev.Delta.StopDetails.Explanation)
			case "pause_turn":
				resp.Stop = "pause"
			}
		case "error":
			status := 500
			if ev.Error.Type == "overloaded_error" {
				status = 529
			}
			return &Error{Status: status, Message: ev.Error.Message}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	var raw []any
	for i, b := range blocks {
		if b == nil {
			continue
		}
		enc, err := json.Marshal(b)
		if err != nil {
			continue
		}
		raw = append(raw, json.RawMessage(enc))
		block := Block{Raw: enc}
		switch str(b["type"]) {
		case "text":
			block.Type, block.Text = Text, str(b["text"])
			if block.Text == "" {
				continue
			}
		case "thinking":
			block.Type, block.Text = Thinking, str(b["thinking"])
		case "tool_use":
			block.Type, block.ID, block.Name = ToolCall, str(b["id"]), str(b["name"])
			block.Input = arguments(partial[i].String())
		case "server_tool_use":
			block.Type, block.Name = Opaque, str(b["name"])
			var in struct{ Query string }
			if rawIn, ok := b["input"].(json.RawMessage); ok && json.Unmarshal(rawIn, &in) == nil {
				block.Text = in.Query
			}
		default:
			block.Type, block.Name = Opaque, str(b["type"])
		}
		resp.Message.Blocks = append(resp.Message.Blocks, block)
	}
	if resp.Stop == StopEnd && len(resp.Message.Calls()) > 0 {
		resp.Stop = StopTool
	}
	return resp, raw, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// orEmptyObject returns s if it is a JSON object, else "{}".
func orEmptyObject(s string) string {
	if s = strings.TrimSpace(s); s != "" && json.Valid([]byte(s)) && strings.HasPrefix(s, "{") {
		return s
	}
	return "{}"
}

// arguments turns the streamed argument text of a tool call into JSON. Text
// that was cut off or is otherwise not an object is kept as a JSON string, so
// the harness can tell the model its call was malformed instead of running it
// with arguments missing.
func arguments(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(s)) && strings.HasPrefix(s, "{") {
		return json.RawMessage(s)
	}
	quoted, _ := json.Marshal(s)
	return quoted
}

// messages converts the conversation to Messages API form. Turns this model
// produced are replayed exactly as it sent them, so signed reasoning stays
// valid; turns from other models are reduced to text and tool calls.
func (a *Anthropic) messages(req Request) []any {
	var out []any
	for _, m := range normalize(req.Messages) {
		var content []any
		own := m.Role == "assistant" && m.Model == req.ID
		for _, b := range m.Blocks {
			switch {
			case own && len(b.Raw) > 0:
				content = append(content, b.Raw)
			case b.Type == Text && b.Text != "":
				content = append(content, map[string]any{"type": "text", "text": b.Text})
			case b.Type == ToolCall:
				content = append(content, map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": json.RawMessage(orEmptyObject(string(b.Input)))})
			case b.Type == ToolResult:
				text := b.Text
				if text == "" {
					text = "(no output)"
				}
				content = append(content, map[string]any{"type": "tool_result", "tool_use_id": b.ID, "content": text, "is_error": b.IsError})
			case b.Type == Image || b.Type == File:
				data, note := encodeFile(b)
				if note != "" {
					content = append(content, map[string]any{"type": "text", "text": note})
					continue
				}
				kind := "image"
				if b.Type == File {
					kind = "document"
				}
				content = append(content, map[string]any{"type": kind, "source": map[string]string{"type": "base64", "media_type": b.MediaType, "data": data}})
			}
		}
		if len(content) == 0 {
			content = []any{map[string]any{"type": "text", "text": "(empty)"}}
		}
		out = append(out, map[string]any{"role": m.Role, "content": content})
	}
	return out
}

func (a *Anthropic) String() string { return fmt.Sprintf("anthropic(%s)", a.Base) }
