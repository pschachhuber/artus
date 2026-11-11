// Package browser implements `artus browser`: an agent-oriented browser CLI
// in the spirit of Playwright CLI, speaking the Chrome DevTools Protocol
// directly to a Chromium-family browser.
//
// A long-lived [Manager] owns browser processes and their DevTools
// connections. Each command is a short argv handed to [Manager.Run], which
// answers with compact text: what happened, the page URL and title, and an
// accessibility snapshot whose element refs (e5, f1e2, ...) address elements
// in later commands.
package browser

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options configures a Manager.
type Options struct {
	// Dir holds config.json, profiles/, chrome/, snapshots/, downloads/ and
	// output/. It is created on demand.
	Dir string
	// ResolveSecret returns the value of a named secret for `--secret NAME`.
	ResolveSecret func(name string) (string, error)
	// TOTP returns the current one-time code of a named secret for
	// `--totp NAME`.
	TOTP func(name string) (string, error)
	// Redact scrubs sensitive values from text before it is returned.
	Redact func(string) string
	// OnEvent receives session, hand-off and download notifications. It may
	// be called from any goroutine.
	OnEvent func(Event)
	// Logf receives diagnostics.
	Logf func(format string, args ...any)
}

// Event is a notification about a browser session.
type Event struct {
	// Type is one of "handoff", "handoff_done", "session_open",
	// "session_close" or "download".
	Type    string `json:"type"`
	Session string `json:"session"`
	ID      string `json:"id,omitempty"`
	Reason  string `json:"reason,omitempty"`
	URL     string `json:"url,omitempty"`
	Path    string `json:"path,omitempty"`
}

// Result is the outcome of a command.
type Result struct {
	// Text is the reply for the agent.
	Text string `json:"text"`
	// Files lists absolute paths of screenshots, PDFs, snapshots and
	// downloads the command produced.
	Files []string `json:"files,omitempty"`
}

// SessionStatus describes a running browser session.
type SessionStatus struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Headless bool   `json:"headless"`
	Tabs     int    `json:"tabs"`
	// Handoff is the pending hand-off to the user, if any.
	Handoff *Event `json:"handoff,omitempty"`
}

// InputEvent is a user input event forwarded from a live view of a session.
type InputEvent struct {
	// Type is mousemove, mousedown, mouseup, wheel, keydown, keyup or text.
	Type string `json:"type"`
	// X and Y are in pixels of the most recent screencast frame.
	X float64 `json:"x"`
	Y float64 `json:"y"`
	// Button is left, right or middle.
	Button string  `json:"button,omitempty"`
	DeltaX float64 `json:"deltaX,omitempty"`
	DeltaY float64 `json:"deltaY,omitempty"`
	// Key and Code follow the DOM KeyboardEvent fields of the same name.
	Key  string `json:"key,omitempty"`
	Code string `json:"code,omitempty"`
	// Text is the text to insert for "text" events.
	Text string `json:"text,omitempty"`
	// Modifiers is a bit mask: Alt=1, Control=2, Meta=4, Shift=8.
	Modifiers int `json:"modifiers,omitempty"`
}

// Manager owns browser sessions and runs commands against them. It is safe
// for concurrent use; commands for the same session run one at a time.
type Manager struct {
	opts Options

	// launchMu serialises starting sessions, so that two commands cannot
	// launch the same profile twice; installMu serialises browser downloads.
	launchMu  sync.Mutex
	installMu sync.Mutex

	mu       sync.Mutex
	sessions map[string]*session
	secrets  map[string]string // secret value -> placeholder shown instead
}

// NewManager returns a Manager storing its state under opts.Dir.
func NewManager(opts Options) *Manager {
	return &Manager{opts: opts, sessions: map[string]*session{}, secrets: map[string]string{}}
}

// Run executes one command. args is everything after "artus browser"; cwd
// resolves relative file arguments.
func (m *Manager) Run(ctx context.Context, cwd string, args []string) (Result, error) {
	res, err := m.run(ctx, cwd, args)
	res.Text = m.redact(res.Text)
	if err != nil {
		return res, errors.New(m.redact(err.Error()))
	}
	return res, nil
}

func (m *Manager) run(ctx context.Context, cwd string, args []string) (Result, error) {
	inv, err := parseArgs(args)
	if err != nil {
		return Result{}, err
	}
	if m.opts.Dir == "" {
		return Result{}, errors.New("browser: Options.Dir is not set")
	}
	cfg, err := loadConfig(m.opts.Dir)
	if err != nil {
		return Result{}, err
	}
	c := &command{m: m, ctx: ctx, cwd: cwd, inv: inv, cfg: cfg, out: &response{}}
	if !inv.spec.needsBrowser {
		if err := inv.spec.run(c); err != nil {
			return Result{}, err
		}
		return c.out.result(c)
	}
	s, err := m.acquire(ctx, c)
	if err != nil {
		return Result{}, err
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	c.s = s
	s.refreshConfig(cfg)
	// A page stuck in script can leave protocol calls unanswered; no command
	// may wait for that indefinitely.
	limit, err := c.limit()
	if err != nil {
		return Result{}, err
	}
	var cancel context.CancelFunc
	c.ctx, cancel = context.WithTimeout(ctx, limit)
	defer cancel()
	if err := inv.spec.run(c); err != nil {
		switch {
		case s.dead():
			return Result{}, fmt.Errorf("the browser of session %q closed during %s; the next command starts it again", s.name, inv.cmd)
		case c.ctx.Err() != nil && ctx.Err() == nil:
			return Result{}, fmt.Errorf("%s: the browser did not respond within %s; the page may be hung (`reload`, `tab-close` or `close` recover)", inv.cmd, limit)
		}
		return Result{}, fmt.Errorf("%s: %w", inv.cmd, err)
	}
	return c.out.result(c)
}

// limit returns the longest a command may run once its browser is up: its
// own timeouts plus generous slack.
func (c *command) limit() (time.Duration, error) {
	override, err := c.inv.timeout(0)
	limit := 30*time.Second + c.cfg.navigationTimeout() + 2*max(c.cfg.actionTimeout(), override)
	if c.inv.cmd == "handoff" {
		limit += max(c.cfg.handoffTimeout(), override)
	}
	return limit, err
}

// Status reports the running sessions, sorted by name.
func (m *Manager) Status() []SessionStatus {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	out := make([]SessionStatus, 0, len(sessions))
	for _, s := range sessions {
		if !s.dead() {
			out = append(out, s.status())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Close shuts down every session: browsers we started exit, their persistent
// profiles stay on disk, and browsers attached through cdpEndpoint are only
// disconnected.
func (m *Manager) Close() {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.close()
		}()
	}
	wg.Wait()
}

// lookup returns the live session with the given name, if any.
func (m *Manager) lookup(name string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[name]; s != nil && !s.dead() {
		return s
	}
	return nil
}

func (m *Manager) emit(ev Event) {
	if m.opts.OnEvent != nil {
		m.opts.OnEvent(ev)
	}
}

func (m *Manager) logf(format string, args ...any) {
	if m.opts.Logf != nil {
		m.opts.Logf(format, args...)
	}
}

// secret resolves a named secret or one-time code and remembers its value so
// that it is scrubbed from everything returned afterwards.
func (m *Manager) secret(name string, totp bool) (string, error) {
	resolve, kind := m.opts.ResolveSecret, "secret"
	if totp {
		resolve, kind = m.opts.TOTP, "TOTP secret"
	}
	if resolve == nil {
		return "", fmt.Errorf("no %s provider is configured", kind)
	}
	value, err := resolve(name)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", kind, name, err)
	}
	if value == "" {
		return "", fmt.Errorf("%s %q is empty", kind, name)
	}
	// Very short values would mangle unrelated output when replaced.
	if len(value) >= 4 {
		placeholder := "[secret:" + name + "]"
		if totp {
			placeholder = "[totp:" + name + "]"
		}
		m.mu.Lock()
		m.secrets[value] = placeholder
		m.mu.Unlock()
	}
	return value, nil
}

// redact removes secret values typed during this process's lifetime, then
// applies the caller's redaction.
func (m *Manager) redact(text string) string {
	if text == "" {
		return text
	}
	m.mu.Lock()
	for value, placeholder := range m.secrets {
		text = strings.ReplaceAll(text, value, placeholder)
	}
	m.mu.Unlock()
	if m.opts.Redact != nil {
		text = m.opts.Redact(text)
	}
	return text
}
