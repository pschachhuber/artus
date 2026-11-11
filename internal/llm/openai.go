package llm

import (
	"context"
	"encoding/json"
	"strings"
)

// OpenAI talks to the OpenAI Responses API, statelessly: nothing is stored on
// the server and the conversation is sent in full on each request, with
// encrypted reasoning items carried along so the model keeps its thread.
type OpenAI struct {
	key    KeyFunc
	Base   string // replaces https://api.openai.com/v1 for Responses-compatible endpoints
	quirks quirks
}

// NewOpenAI returns the provider for api.openai.com.
func NewOpenAI(key KeyFunc) *OpenAI {
	return &OpenAI{key: key, Base: "https://api.openai.com/v1"}
}

func (o *OpenAI) headers(ctx context.Context) (map[string]string, error) {
	key, err := o.key(ctx)
	if err != nil {
		return nil, &Error{Status: 401, Message: err.Error()}
	}
	return map[string]string{"Authorization": "Bearer " + key}, nil
}

// Models implements Provider. The API does not describe its models, so the
// list is narrowed to ones that can hold a conversation by name.
func (o *OpenAI) Models(ctx context.Context) ([]Model, error) {
	h, err := o.headers(ctx)
	if err != nil {
		return nil, err
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, o.Base+"/models", h, &list); err != nil {
		return nil, err
	}
	var out []Model
	for _, m := range list.Data {
		if openaiChatModel(m.ID) {
			out = append(out, Model{ID: m.ID, Name: m.ID, Vision: true, Reasoning: true})
		}
	}
	return out, nil
}

func openaiChatModel(id string) bool {
	for _, not := range []string{"embedding", "whisper", "tts", "dall-e", "image", "audio", "realtime", "moderation", "transcribe", "sora", "babbage", "davinci", "search", "instruct"} {
		if strings.Contains(id, not) {
			return false
		}
	}
	return strings.HasPrefix(id, "gpt-") || strings.HasPrefix(id, "o") || strings.HasPrefix(id, "chatgpt") || strings.Contains(id, "codex")
}

var openaiFeatures = []feature{
	{"xhigh", []string{"xhigh"}},
	{"search", []string{"web_search"}},
	{"include", []string{"encrypted_content", "'include'", "include["}},
	{"cache_key", []string{"prompt_cache_key"}},
	{"summary", []string{"summary"}},
	{"reasoning", []string{"reasoning"}},
}

// Stream implements Provider.
func (o *OpenAI) Stream(ctx context.Context, req Request, emit func(Delta)) (*Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := o.send(ctx, req, emit)
		if f := rejected(err, openaiFeatures); f != "" && attempt < len(openaiFeatures) && !o.quirks.disabled(req.Model, f) {
			o.quirks.disable(req.Model, f)
			continue
		}
		return resp, err
	}
}

func (o *OpenAI) send(ctx context.Context, req Request, emit func(Delta)) (*Response, error) {
	headers, err := o.headers(ctx)
	if err != nil {
		return nil, err
	}
	on := func(f string) bool { return !o.quirks.disabled(req.Model, f) }
	body := map[string]any{"model": req.Model, "stream": true, "store": false, "input": o.input(req)}
	if req.System != "" {
		body["instructions"] = req.System
	}
	if on("include") {
		body["include"] = []string{"reasoning.encrypted_content"}
	}
	if on("cache_key") && req.Cache != "" {
		body["prompt_cache_key"] = req.Cache
	}
	if on("reasoning") {
		levels := []string{"low", "medium", "high"}
		if on("xhigh") {
			levels = append(levels, "xhigh")
		}
		reasoning := map[string]string{"effort": ClampEffort(req.Effort, levels...)}
		if on("summary") {
			reasoning["summary"] = "auto"
		}
		body["reasoning"] = reasoning
	}
	var tools []any
	for _, t := range req.Tools {
		tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Schema, "strict": false})
	}
	if req.WebSearch && on("search") {
		tools = append(tools, map[string]any{"type": "web_search"})
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}

	resp := &Response{Message: Message{Role: "assistant"}, Stop: StopEnd}
	calls := map[string]string{} // output item id -> call id
	err = postStream(ctx, o.Base+"/responses", headers, body, func(_ string, data []byte) error {
		var ev struct {
			Type   string          `json:"type"`
			Delta  string          `json:"delta"`
			ItemID string          `json:"item_id"`
			Item   json.RawMessage `json:"item"`
			// error events
			Message string `json:"message"`
			Code    string `json:"code"`
			// terminal events
			Response struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
					Code    string `json:"code"`
				} `json:"error"`
				Incomplete *struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
				Usage struct {
					Input   int     `json:"input_tokens"`
					Output  int     `json:"output_tokens"`
					Cost    float64 `json:"cost"`
					Details struct {
						Cached int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return nil
		}
		switch ev.Type {
		case "response.output_text.delta":
			emit(Delta{Kind: Text, Text: ev.Delta})
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			emit(Delta{Kind: Thinking, Text: ev.Delta})
		case "response.function_call_arguments.delta":
			emit(Delta{Kind: "tool", ID: calls[ev.ItemID], Text: ev.Delta})
		case "response.output_item.added":
			var item struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				Name   string `json:"name"`
				CallID string `json:"call_id"`
			}
			json.Unmarshal(ev.Item, &item)
			switch item.Type {
			case "function_call":
				calls[item.ID] = item.CallID
				emit(Delta{Kind: "tool", ID: item.CallID, Name: item.Name})
			case "web_search_call":
				emit(Delta{Kind: "tool", ID: item.ID, Name: "web_search"})
			}
		case "response.output_item.done":
			if b, refusal := openaiBlock(ev.Item); b != nil {
				resp.Message.Blocks = append(resp.Message.Blocks, *b)
			} else if refusal != "" {
				resp.Stop, resp.Detail = StopRefusal, refusal
			}
		case "response.completed", "response.incomplete":
			u := ev.Response.Usage
			resp.Usage = Usage{Input: u.Input, Output: u.Output, CacheRead: u.Details.Cached, Cost: u.Cost}
			if ev.Response.Incomplete != nil && strings.Contains(ev.Response.Incomplete.Reason, "max_output_tokens") {
				resp.Stop = StopLength
			}
		case "response.failed":
			msg := "the request failed"
			if ev.Response.Error != nil {
				msg = ev.Response.Error.Message
			}
			return &Error{Status: 500, Message: msg}
		case "error":
			status := 500
			if strings.Contains(ev.Code, "rate_limit") {
				status = 429
			} else if strings.Contains(ev.Code, "context_length") || strings.Contains(ev.Code, "invalid") {
				status = 400
			}
			return &Error{Status: status, Message: ev.Message}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if resp.Stop == StopEnd && len(resp.Message.Calls()) > 0 {
		resp.Stop = StopTool
	}
	return resp, nil
}

// openaiBlock converts a finished output item. A refusal comes back as text
// in the second result instead of a block.
func openaiBlock(raw json.RawMessage) (*Block, string) {
	var item struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
		Summary   []struct {
			Text string `json:"text"`
		} `json:"summary"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
		Action struct {
			Query string `json:"query"`
		} `json:"action"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return nil, ""
	}
	switch item.Type {
	case "reasoning":
		var parts []string
		for _, s := range item.Summary {
			parts = append(parts, s.Text)
		}
		for _, c := range item.Content {
			if c.Type == "reasoning_text" {
				parts = append(parts, c.Text)
			}
		}
		return &Block{Type: Thinking, Text: strings.Join(parts, "\n\n"), Raw: raw}, ""
	case "message":
		var text strings.Builder
		for _, c := range item.Content {
			if c.Type == "refusal" && c.Refusal != "" {
				return nil, c.Refusal
			}
			text.WriteString(c.Text)
		}
		if text.Len() == 0 {
			return nil, ""
		}
		return &Block{Type: Text, Text: text.String()}, ""
	case "function_call":
		return &Block{Type: ToolCall, ID: item.CallID, Name: item.Name, Input: arguments(item.Arguments)}, ""
	case "web_search_call":
		return &Block{Type: Opaque, Name: "web_search", Text: item.Action.Query, Raw: raw}, ""
	}
	return &Block{Type: Opaque, Name: item.Type, Raw: raw}, ""
}

// input converts the conversation to Responses input items. Reasoning items
// this model produced are replayed as received; everything else is rebuilt.
func (o *OpenAI) input(req Request) []any {
	var items []any
	for _, m := range normalize(req.Messages) {
		own := m.Role == "assistant" && m.Model == req.ID
		var parts []any // content of the message item being built
		flush := func() {
			if len(parts) > 0 {
				items = append(items, map[string]any{"role": m.Role, "content": parts})
				parts = nil
			}
		}
		for _, b := range m.Blocks {
			switch {
			case b.Type == Text && b.Text != "":
				kind := "input_text"
				if m.Role == "assistant" {
					kind = "output_text"
				}
				parts = append(parts, map[string]string{"type": kind, "text": b.Text})
			case b.Type == Image || b.Type == File:
				url, note := dataURL(b)
				switch {
				case note != "":
					parts = append(parts, map[string]string{"type": "input_text", "text": note})
				case b.Type == Image:
					parts = append(parts, map[string]string{"type": "input_image", "image_url": url})
				default:
					parts = append(parts, map[string]string{"type": "input_file", "filename": b.Name, "file_data": url})
				}
			case b.Type == ToolCall:
				flush()
				items = append(items, map[string]string{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": orEmptyObject(string(b.Input))})
			case b.Type == ToolResult:
				flush()
				text := b.Text
				if text == "" {
					text = "(no output)"
				}
				items = append(items, map[string]string{"type": "function_call_output", "call_id": b.ID, "output": text})
			case own && len(b.Raw) > 0 && (b.Type == Thinking || b.Type == Opaque):
				flush()
				items = append(items, b.Raw)
			}
		}
		flush()
	}
	return items
}
