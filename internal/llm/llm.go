// Package llm is the provider-neutral model interface: one message format,
// one streaming call, and adapters for Anthropic, OpenAI and OpenRouter.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Block types.
const (
	Text       = "text"
	Thinking   = "thinking"    // reasoning summary shown to the user
	ToolCall   = "tool_call"   // the model asks for a tool to run
	ToolResult = "tool_result" // what the tool returned
	Image      = "image"       // an image file on disk
	File       = "file"        // a PDF on disk
	Opaque     = "opaque"      // provider-native block with nothing to show (encrypted reasoning, server tool traffic)
)

// Block is one piece of a message.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`    // tool call id; on a result, the call it answers
	Name      string          `json:"name,omitempty"`  // tool name
	Input     json.RawMessage `json:"input,omitempty"` // tool arguments
	IsError   bool            `json:"isError,omitempty"`
	Path      string          `json:"path,omitempty"` // image or file location
	MediaType string          `json:"mediaType,omitempty"`
	// Raw is the block exactly as the provider sent it. It is replayed
	// verbatim to the same model so signed reasoning stays valid.
	Raw json.RawMessage `json:"raw,omitempty"`
	// UI carries display-only details (duration, diff, sub-agent id). Never sent to a model.
	UI json.RawMessage `json:"ui,omitempty"`
}

// Message is one turn of a conversation.
type Message struct {
	Role   string  `json:"role"` // user | assistant
	Blocks []Block `json:"blocks"`
	Model  string  `json:"model,omitempty"` // "provider/model" that wrote an assistant message
	// Synthetic marks user-role messages the harness wrote (wake-ups, compaction prompts).
	Synthetic bool `json:"synthetic,omitempty"`
}

// TextOf joins the text blocks of a message.
func (m Message) TextOf() string {
	var parts []string
	for _, b := range m.Blocks {
		if b.Type == Text && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Calls returns the tool calls in a message.
func (m Message) Calls() []Block {
	var out []Block
	for _, b := range m.Blocks {
		if b.Type == ToolCall {
			out = append(out, b)
		}
	}
	return out
}

// UserText builds a plain user message.
func UserText(text string) Message {
	return Message{Role: "user", Blocks: []Block{{Type: Text, Text: text}}}
}

// Tool describes a tool the model may call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"` // JSON Schema of the arguments
}

// Request is one model call.
type Request struct {
	ID        string // full "provider/model" id; turns stamped with it were written by this model
	Model     string // provider-local id, without the "provider/" prefix
	Effort    string // low | medium | high | xhigh | max
	System    string
	Messages  []Message
	Tools     []Tool
	WebSearch bool   // enable the provider's built-in web search
	Cache     string // stable key grouping requests of one conversation
}

// Usage reports the tokens of one call.
type Usage struct {
	Input      int     `json:"input"`  // all prompt tokens, cached or not
	Output     int     `json:"output"` // generated tokens, reasoning included
	CacheRead  int     `json:"cacheRead,omitempty"`
	CacheWrite int     `json:"cacheWrite,omitempty"`
	Cost       float64 `json:"cost,omitempty"` // in USD, when the provider reports it
}

// Stop reasons.
const (
	StopEnd     = "end"
	StopTool    = "tool"
	StopLength  = "length"
	StopRefusal = "refusal"
)

// Response is the complete result of a call.
type Response struct {
	Message Message
	Usage   Usage
	Stop    string
	Detail  string // explanation for a refusal
}

// Delta is a streamed fragment of the response in progress.
type Delta struct {
	Kind string `json:"kind"` // text | thinking | tool
	Text string `json:"text,omitempty"`
	ID   string `json:"id,omitempty"`   // tool: call id
	Name string `json:"name,omitempty"` // tool: tool name, sent once when the call starts
}

// Model describes an available model.
type Model struct {
	ID        string  `json:"id"` // "provider/model-id"
	Name      string  `json:"name"`
	Window    int     `json:"window,omitempty"`    // context window in tokens
	Vision    bool    `json:"vision,omitempty"`    // accepts images
	Reasoning bool    `json:"reasoning,omitempty"` // has an effort setting
	PriceIn   float64 `json:"priceIn,omitempty"`   // USD per million input tokens
	PriceOut  float64 `json:"priceOut,omitempty"`  // USD per million output tokens
}

// Provider talks to one model vendor.
type Provider interface {
	// Stream runs one request, calling emit for each fragment as it arrives.
	Stream(ctx context.Context, req Request, emit func(Delta)) (*Response, error)
	// Models lists what the account can use.
	Models(ctx context.Context) ([]Model, error)
}

// Error is a failed provider call.
type Error struct {
	Status  int // HTTP status, 0 for transport errors
	Message string
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Message
	}
	return fmt.Sprintf("%d: %s", e.Status, e.Message)
}

// Retryable reports whether trying again may succeed.
func (e *Error) Retryable() bool {
	return e.Status == 0 || e.Status == 408 || e.Status == 409 || e.Status == 429 || e.Status >= 500
}

// Efforts are the reasoning tiers, lowest first.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// ClampEffort maps effort onto the nearest tier a provider supports.
func ClampEffort(effort string, supported ...string) string {
	rank := func(e string) int {
		for i, v := range Efforts {
			if v == e {
				return i
			}
		}
		return 1
	}
	best, want := "", rank(effort)
	for _, s := range supported {
		if best == "" || abs(rank(s)-want) < abs(rank(best)-want) {
			best = s
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Registry routes "provider/model" ids to providers and caches model lists.
type Registry struct {
	mu        sync.Mutex
	providers map[string]Provider
	models    map[string][]Model
	fetched   map[string]time.Time
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}, models: map[string][]Model{}, fetched: map[string]time.Time{}}
}

// Register adds a provider under a name such as "anthropic".
func (r *Registry) Register(name string, p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = p
}

// Names lists the registered providers.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Resolve splits "provider/model" and returns the provider and its local model id.
func (r *Registry) Resolve(id string) (Provider, string, error) {
	name, model, ok := strings.Cut(id, "/")
	if !ok || model == "" {
		return nil, "", fmt.Errorf("model %q must look like provider/model", id)
	}
	r.mu.Lock()
	p := r.providers[name]
	r.mu.Unlock()
	if p == nil {
		return nil, "", fmt.Errorf("unknown provider %q", name)
	}
	return p, model, nil
}

// Models returns a provider's models with ids prefixed by the provider name,
// refreshing the cached list at most every ten minutes.
func (r *Registry) Models(ctx context.Context, name string, refresh bool) ([]Model, error) {
	r.mu.Lock()
	p, cached, at := r.providers[name], r.models[name], r.fetched[name]
	r.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	if cached != nil && !refresh && time.Since(at) < 10*time.Minute {
		return cached, nil
	}
	models, err := p.Models(ctx)
	if err != nil {
		return cached, err
	}
	for i := range models {
		models[i].ID = name + "/" + models[i].ID
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	r.mu.Lock()
	r.models[name], r.fetched[name] = models, time.Now()
	r.mu.Unlock()
	return models, nil
}

// Info looks a model up in the cached lists, fetching its provider's list once if needed.
func (r *Registry) Info(ctx context.Context, id string) (Model, bool) {
	name, _, _ := strings.Cut(id, "/")
	models, _ := r.Models(ctx, name, false)
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}
