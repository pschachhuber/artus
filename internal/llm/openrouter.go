package llm

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// OpenRouter talks to OpenRouter's chat completions API, which fronts models
// from many vendors behind one format.
type OpenRouter struct {
	key    KeyFunc
	Base   string
	quirks quirks

	mu   sync.Mutex
	info map[string]openrouterModel
}

// NewOpenRouter returns the provider for openrouter.ai.
func NewOpenRouter(key KeyFunc) *OpenRouter {
	return &OpenRouter{key: key, Base: "https://openrouter.ai/api/v1", info: map[string]openrouterModel{}}
}

type openrouterModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ContextLength int      `json:"context_length"`
	Parameters    []string `json:"supported_parameters"`
	Architecture  struct {
		Input []string `json:"input_modalities"`
	} `json:"architecture"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
}

func (m openrouterModel) supports(param string) bool {
	for _, p := range m.Parameters {
		if p == param {
			return true
		}
	}
	return false
}

func (o *OpenRouter) headers(ctx context.Context) (map[string]string, error) {
	key, err := o.key(ctx)
	if err != nil {
		return nil, &Error{Status: 401, Message: err.Error()}
	}
	return map[string]string{"Authorization": "Bearer " + key}, nil
}

// Models implements Provider. Only models that can call tools are listed:
// the harness is of no use without them.
func (o *OpenRouter) Models(ctx context.Context) ([]Model, error) {
	var list struct {
		Data []openrouterModel `json:"data"`
	}
	if err := getJSON(ctx, o.Base+"/models", nil, &list); err != nil {
		return nil, err
	}
	perMillion := func(s string) float64 {
		f, _ := strconv.ParseFloat(s, 64)
		return f * 1e6
	}
	var out []Model
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, m := range list.Data {
		o.info[m.ID] = m
		if !m.supports("tools") {
			continue
		}
		vision := false
		for _, in := range m.Architecture.Input {
			vision = vision || in == "image"
		}
		out = append(out, Model{ID: m.ID, Name: m.Name, Window: m.ContextLength, Vision: vision, Reasoning: m.supports("reasoning"),
			PriceIn: perMillion(m.Pricing.Prompt), PriceOut: perMillion(m.Pricing.Completion)})
	}
	return out, nil
}

func (o *OpenRouter) model(ctx context.Context, id string) (openrouterModel, bool) {
	o.mu.Lock()
	m, ok := o.info[id]
	empty := len(o.info) == 0
	o.mu.Unlock()
	if !ok && empty {
		if _, err := o.Models(ctx); err == nil {
			o.mu.Lock()
			m, ok = o.info[id]
			o.mu.Unlock()
		}
	}
	return m, ok
}

var openrouterFeatures = []feature{
	{"search", []string{"openrouter:web_search", "web_search", "server tool"}},
	{"cache_control", []string{"cache_control"}},
	{"xhigh", []string{"xhigh"}},
	{"reasoning", []string{"reasoning"}},
}

// Stream implements Provider.
func (o *OpenRouter) Stream(ctx context.Context, req Request, emit func(Delta)) (*Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := o.send(ctx, req, emit)
		if f := rejected(err, openrouterFeatures); f != "" && attempt < len(openrouterFeatures) && !o.quirks.disabled(req.Model, f) {
			o.quirks.disable(req.Model, f)
			continue
		}
		return resp, err
	}
}

func (o *OpenRouter) send(ctx context.Context, req Request, emit func(Delta)) (*Response, error) {
	headers, err := o.headers(ctx)
	if err != nil {
		return nil, err
	}
	on := func(f string) bool { return !o.quirks.disabled(req.Model, f) }
	info, known := o.model(ctx, req.Model)
	// Anthropic and Gemini models need explicit cache breakpoints; the others cache on their own.
	explicitCache := on("cache_control") && (strings.HasPrefix(req.Model, "anthropic/") || strings.HasPrefix(req.Model, "google/"))

	body := map[string]any{
		"model": req.Model, "stream": true, "usage": map[string]bool{"include": true},
		"messages": o.messages(req, explicitCache, !known || contains(info.Architecture.Input, "image"), !known || contains(info.Architecture.Input, "file")),
	}
	if on("reasoning") && (!known || info.supports("reasoning")) {
		levels := []string{"low", "medium", "high"}
		if on("xhigh") {
			levels = append(levels, "xhigh")
		}
		body["reasoning"] = map[string]string{"effort": ClampEffort(req.Effort, levels...)}
	}
	var tools []any
	for _, t := range req.Tools {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema}})
	}
	if req.WebSearch && on("search") {
		tools = append(tools, map[string]any{"type": "openrouter:web_search"})
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}

	type call struct {
		id, name string
		args     strings.Builder
	}
	var text, thinking strings.Builder
	var calls []*call
	searches := 0
	details := map[int]map[string]any{}
	resp := &Response{Message: Message{Role: "assistant"}, Stop: StopEnd}
	err = postStream(ctx, o.Base+"/chat/completions", headers, body, func(_ string, data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		var chunk struct {
			Error *struct {
				Message  string          `json:"message"`
				Code     any             `json:"code"`
				Metadata json.RawMessage `json:"metadata"`
			} `json:"error"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content          string           `json:"content"`
					Reasoning        string           `json:"reasoning"`
					ReasoningDetails []map[string]any `json:"reasoning_details"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int     `json:"prompt_tokens"`
				Completion int     `json:"completion_tokens"`
				Cost       float64 `json:"cost"`
				Details    struct {
					Cached int `json:"cached_tokens"`
					Write  int `json:"cache_write_tokens"`
				} `json:"prompt_tokens_details"`
				ServerTools struct {
					WebSearches int `json:"web_search_requests"`
				} `json:"server_tool_use_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			return nil
		}
		if chunk.Error != nil {
			status := 500
			switch code := chunk.Error.Code.(type) {
			case float64:
				status = int(code)
			case string:
				if n, err := strconv.Atoi(code); err == nil {
					status = n
				}
			}
			raw, _ := json.Marshal(map[string]any{"error": chunk.Error})
			return &Error{Status: status, Message: errorMessage(raw, chunk.Error.Message)}
		}
		if u := chunk.Usage; u != nil {
			resp.Usage = Usage{Input: u.Prompt, Output: u.Completion, CacheRead: u.Details.Cached, CacheWrite: u.Details.Write, Cost: u.Cost}
			searches = u.ServerTools.WebSearches
		}
		for _, choice := range chunk.Choices {
			d := choice.Delta
			if d.Content != "" {
				text.WriteString(d.Content)
				emit(Delta{Kind: Text, Text: d.Content})
			}
			if d.Reasoning != "" {
				thinking.WriteString(d.Reasoning)
				emit(Delta{Kind: Thinking, Text: d.Reasoning})
			}
			for _, rd := range d.ReasoningDetails {
				mergeReasoning(details, rd)
			}
			for _, tc := range d.ToolCalls {
				for len(calls) <= tc.Index {
					calls = append(calls, &call{})
				}
				c := calls[tc.Index]
				if tc.ID != "" {
					c.id = tc.ID
				}
				if tc.Function.Name != "" && c.name == "" {
					c.name = tc.Function.Name
					emit(Delta{Kind: "tool", ID: c.id, Name: c.name})
				}
				if tc.Function.Arguments != "" {
					c.args.WriteString(tc.Function.Arguments)
					emit(Delta{Kind: "tool", ID: c.id, Text: tc.Function.Arguments})
				}
			}
			switch choice.FinishReason {
			case "length":
				resp.Stop = StopLength
			case "content_filter":
				resp.Stop, resp.Detail = StopRefusal, "The provider's content filter stopped the reply."
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Reasoning comes first so it replays ahead of the text and calls it led to.
	if len(details) > 0 || thinking.Len() > 0 {
		b := Block{Type: Thinking, Text: thinking.String()}
		if len(details) > 0 {
			var order []int
			for i := range details {
				order = append(order, i)
			}
			sort.Ints(order)
			list := make([]map[string]any, 0, len(order))
			for _, i := range order {
				list = append(list, details[i])
			}
			b.Raw, _ = json.Marshal(list)
		}
		resp.Message.Blocks = append(resp.Message.Blocks, b)
	}
	if searches > 0 {
		// The search ran on the provider's side and leaves nothing in the reply
		// itself. Record that it happened: see searchNote.
		resp.Message.Blocks = append(resp.Message.Blocks, Block{Type: Opaque, Name: "web_search", Text: searchCount(searches)})
	}
	if text.Len() > 0 {
		resp.Message.Blocks = append(resp.Message.Blocks, Block{Type: Text, Text: text.String()})
	}
	for _, c := range calls {
		if c.name == "" {
			continue
		}
		resp.Message.Blocks = append(resp.Message.Blocks, Block{Type: ToolCall, ID: c.id, Name: c.name, Input: arguments(c.args.String())})
		if resp.Stop == StopEnd {
			resp.Stop = StopTool
		}
	}
	return resp, nil
}

func searchCount(n int) string {
	if n == 1 {
		return "1 web search"
	}
	return strconv.Itoa(n) + " web searches"
}

// searchNote tells the model that an earlier reply of its own rested on web
// searches. Chat completions carry no trace of a provider-side search, and a
// model that sees a factual answer with no search behind it will disown the
// answer as made up. The note rides on the next user turn.
func searchNote(count string) map[string]any {
	return map[string]any{"type": "text", "text": "<harness>Your previous reply was based on " + count +
		" run by the provider. The results are not kept in this history.</harness>"}
}

// mergeReasoning folds one streamed reasoning_details fragment into the
// entry with the same index. These entries carry the signatures and
// encrypted payloads a model needs to see again on the next request.
func mergeReasoning(details map[int]map[string]any, part map[string]any) {
	index := 0
	if f, ok := part["index"].(float64); ok {
		index = int(f)
	}
	cur := details[index]
	if cur == nil {
		details[index] = part
		return
	}
	for k, v := range part {
		s, isText := v.(string)
		old, had := cur[k].(string)
		if isText && had && (k == "text" || k == "summary" || k == "data") {
			cur[k] = old + s
		} else if v != nil {
			cur[k] = v
		}
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// messages converts the conversation to chat completions form.
func (o *OpenRouter) messages(req Request, cache, vision, files bool) []any {
	var out []any
	if req.System != "" {
		part := map[string]any{"type": "text", "text": req.System}
		if cache {
			part["cache_control"] = map[string]string{"type": "ephemeral"}
		}
		out = append(out, map[string]any{"role": "system", "content": []any{part}})
	}
	searched := "" // set by an assistant turn that used web search, consumed by the next user turn
	for _, m := range normalize(req.Messages) {
		if m.Role == "assistant" {
			msg := map[string]any{"role": "assistant"}
			var calls []any
			for _, b := range m.Blocks {
				switch b.Type {
				case Opaque:
					if b.Name == "web_search" {
						searched = b.Text
					}
				case Text:
					msg["content"] = str(msg["content"]) + b.Text
				case ToolCall:
					calls = append(calls, map[string]any{"id": b.ID, "type": "function", "function": map[string]string{"name": b.Name, "arguments": orEmptyObject(string(b.Input))}})
				case Thinking:
					if m.Model == req.ID && len(b.Raw) > 0 {
						msg["reasoning_details"] = b.Raw
					}
				}
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			if _, ok := msg["content"]; !ok {
				msg["content"] = nil
			}
			out = append(out, msg)
			continue
		}
		var parts []any
		for _, b := range m.Blocks {
			switch b.Type {
			case ToolResult:
				text := b.Text
				if text == "" {
					text = "(no output)"
				}
				out = append(out, map[string]any{"role": "tool", "tool_call_id": b.ID, "content": text})
			case Text:
				if b.Text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": b.Text})
				}
			case Image, File:
				url, note := dataURL(b)
				switch {
				case note != "":
					parts = append(parts, map[string]any{"type": "text", "text": note})
				case b.Type == Image && vision:
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
				case b.Type == File && files:
					parts = append(parts, map[string]any{"type": "file", "file": map[string]string{"filename": b.Name, "file_data": url}})
				default:
					parts = append(parts, map[string]any{"type": "text", "text": "[" + b.Path + " cannot be shown to this model; inspect it with tools]"})
				}
			}
		}
		if searched != "" {
			parts = append([]any{searchNote(searched)}, parts...)
			searched = ""
		}
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": parts})
		}
	}
	if cache { // a second breakpoint at the end caches the conversation so far
		for i := len(out) - 1; i > 0; i-- {
			msg, _ := out[i].(map[string]any)
			if parts, ok := msg["content"].([]any); ok && len(parts) > 0 {
				if last, ok := parts[len(parts)-1].(map[string]any); ok && last["type"] == "text" {
					last["cache_control"] = map[string]string{"type": "ephemeral"}
					break
				}
			}
			if text, ok := msg["content"].(string); ok && text != "" && msg["role"] == "tool" {
				msg["content"] = []any{map[string]any{"type": "text", "text": text, "cache_control": map[string]string{"type": "ephemeral"}}}
				break
			}
		}
	}
	return out
}
