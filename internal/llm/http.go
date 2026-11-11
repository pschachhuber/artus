package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// KeyFunc returns the API key for a provider at the moment of a request, so
// a key added or replaced in Settings takes effect without a restart.
type KeyFunc func(ctx context.Context) (string, error)

// httpClient has no overall timeout: a streamed answer may legitimately run
// for many minutes. Cancellation comes from the request context.
var httpClient = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ResponseHeaderTimeout: 10 * time.Minute,
	IdleConnTimeout:       90 * time.Second,
	ForceAttemptHTTP2:     true,
}}

// postStream POSTs a JSON body and calls each for every server-sent event of
// the response. A non-2xx status becomes an *Error. Whether the stream got
// to its end is for the caller to judge: each provider has its own closing
// event, and a stream without it is reported with cutOff.
func postStream(ctx context.Context, url string, headers map[string]string, body any, each func(event string, data []byte) error) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Message: "connection failed: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}
	r := bufio.NewReaderSize(resp.Body, 1<<16)
	var event string
	var data []byte
	for {
		line, err := r.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0:
			if len(data) > 0 {
				if herr := each(event, data); herr != nil {
					return herr
				}
			}
			event, data = "", nil
		case line[0] == ':': // comment or keep-alive
		case bytes.HasPrefix(line, []byte("event:")):
			event = string(bytes.TrimSpace(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")):
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(line[5:], []byte(" "))...)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == io.EOF {
				if len(data) > 0 { // a last event without its closing blank line
					return each(event, data)
				}
				return nil
			}
			return &Error{Message: "the response stream broke off: " + err.Error()}
		}
	}
}

// cutOff is the error for a stream that ended before the provider's closing
// event, as when a proxy drops the connection mid-reply. It has no status,
// so it counts as retryable.
func cutOff() *Error {
	return &Error{Message: "the connection closed before the reply was complete"}
}

// getJSON fetches and decodes a JSON document.
func getJSON(ctx context.Context, url string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return &Error{Message: "connection failed: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

// apiError reads the error body of a failed response. Providers agree on
// {"error": {"message": …}} closely enough to share this.
func apiError(resp *http.Response) *Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return &Error{Status: resp.StatusCode, Message: errorMessage(body, resp.Status)}
}

func errorMessage(body []byte, fallback string) string {
	var shape struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &shape) == nil {
		var detail struct {
			Message  string          `json:"message"`
			Metadata json.RawMessage `json:"metadata"`
		}
		var text string
		switch {
		case json.Unmarshal(shape.Error, &detail) == nil && detail.Message != "":
			msg := detail.Message
			var meta struct {
				Raw string `json:"raw"`
			}
			if json.Unmarshal(detail.Metadata, &meta) == nil && meta.Raw != "" { // OpenRouter wraps the upstream error
				msg += ": " + errorMessage([]byte(meta.Raw), meta.Raw)
			}
			return msg
		case json.Unmarshal(shape.Error, &text) == nil && text != "":
			return text
		case shape.Message != "":
			return shape.Message
		}
	}
	if text := strings.TrimSpace(string(body)); text != "" && len(text) < 600 && !strings.HasPrefix(text, "<") {
		return text
	}
	return fallback
}

// quirks remembers, per model, optional request features an endpoint said it
// does not support, so one failed attempt teaches the adapter to stop sending
// them. This keeps the adapters free of per-model tables that go stale.
type quirks struct {
	mu  sync.Mutex
	off map[string]bool
}

func (q *quirks) disabled(model, feature string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.off[model+"|"+feature]
}

func (q *quirks) disable(model, feature string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.off == nil {
		q.off = map[string]bool{}
	}
	q.off[model+"|"+feature] = true
}

// feature is an optional request field and the words that name it in an
// error message.
type feature struct {
	name  string
	hints []string
}

// unsupported are the words with which an endpoint says it does not take a
// parameter at all, as opposed to objecting to what one request carries.
// Only such an error is remembered for the model.
var unsupported = []string{"not supported", "unsupported", "does not support", "is not available for this model",
	"unknown parameter", "unrecognized", "extra inputs are not permitted", "expected tags", "supported values",
	"unexpected value", // an anthropic-beta header the endpoint does not know
	"no endpoints found that support", "must be verified"}

// rejected returns the first feature, in order, that a client-error response
// says the endpoint does not support.
func rejected(err error, features []feature) string {
	e, ok := err.(*Error)
	if !ok || (e.Status != 400 && e.Status != 404 && e.Status != 422) {
		return ""
	}
	msg := strings.ToLower(e.Message)
	if !mentions(msg, unsupported) {
		return ""
	}
	for _, f := range features {
		if mentions(msg, f.hints) {
			return f.name
		}
	}
	return ""
}

// staleReasoning reports a 400 about reasoning carried over from earlier
// turns: a thinking signature or encrypted reasoning that no longer verifies
// (an edited history, a key from another organization), or a reasoning item
// out of place. It concerns one conversation, not the model, so it is not
// remembered: the request is tried once more with withoutReasoning.
func staleReasoning(err error) bool {
	e, ok := err.(*Error)
	if !ok || e.Status != 400 {
		return false
	}
	msg := strings.ToLower(e.Message)
	return !mentions(msg, unsupported) && mentions(msg, []string{"signature", "redacted_thinking", "encrypted content", "encrypted_content", "type 'reasoning'"})
}

// withoutReasoning returns a copy of req whose assistant turns leave out
// their reasoning. The stored conversation is not changed.
func withoutReasoning(req Request) Request {
	msgs := make([]Message, len(req.Messages))
	for i, m := range req.Messages {
		if m.Role == "assistant" {
			var kept []Block
			for _, b := range m.Blocks {
				if b.Type != Thinking && !(b.Type == Opaque && b.Name == "redacted_thinking") {
					kept = append(kept, b)
				}
			}
			m.Blocks = kept
		}
		msgs[i] = m
	}
	req.Messages = msgs
	return req
}

// finish settles the stop reason of a complete reply: one with tool calls
// stops for them, and a declined one hands out no calls at all.
func finish(resp *Response) {
	switch {
	case resp.Stop == StopRefusal:
		var kept []Block
		for _, b := range resp.Message.Blocks {
			if b.Type != ToolCall {
				kept = append(kept, b)
			}
		}
		resp.Message.Blocks = kept
	case resp.Stop == StopEnd && len(resp.Message.Calls()) > 0:
		resp.Stop = StopTool
	}
}

func mentions(msg string, words []string) bool {
	for _, w := range words {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}

// dataURL renders an attachment as a data: URL, or returns a note for the
// model when it cannot be attached.
func dataURL(b Block) (url, note string) {
	data, note := encodeFile(b)
	if note != "" {
		return "", note
	}
	return "data:" + b.MediaType + ";base64," + data, ""
}

const maxAttachment = 20 << 20

// encodeFile returns a file's content as base64, or a note for the model
// when it cannot be attached.
func encodeFile(b Block) (data, note string) {
	raw, err := os.ReadFile(b.Path)
	switch {
	case err != nil:
		return "", fmt.Sprintf("[attachment %s is no longer on disk]", b.Path)
	case len(raw) > maxAttachment:
		return "", fmt.Sprintf("[attachment %s is too large to show (%d MB); inspect it with tools]", b.Path, len(raw)>>20)
	}
	return base64.StdEncoding.EncodeToString(raw), ""
}

// normalize prepares messages for an API: consecutive messages of one role
// are merged (APIs want strict alternation; queued user messages and tool
// results otherwise arrive back to back) and, within a user turn, tool
// results are moved ahead of everything else.
func normalize(msgs []Message) []Message {
	var out []Message
	for _, m := range msgs {
		if len(m.Blocks) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == m.Role && (m.Role == "user" || out[n-1].Model == m.Model) {
			out[n-1].Blocks = append(append([]Block(nil), out[n-1].Blocks...), m.Blocks...)
			continue
		}
		out = append(out, m)
	}
	for i, m := range out {
		if m.Role != "user" {
			continue
		}
		var results, rest []Block
		for _, b := range m.Blocks {
			if b.Type == ToolResult {
				results = append(results, b)
			} else {
				rest = append(rest, b)
			}
		}
		out[i].Blocks = append(results, rest...)
	}
	return out
}
