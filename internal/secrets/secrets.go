// Package secrets is the registry of named secrets in the home's .env file.
// A value is either a literal or a Proton Pass reference (pass://vault/item/field)
// resolved on demand through pass-cli. The model works with names only: values
// are injected into subprocesses and scrubbed from everything it reads back.
package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reserved names hold the harness's own credentials. They are never injected
// into commands the agent runs and cannot be added or replaced by it.
var Reserved = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", PassTokenName}

// PassTokenName is the secret holding the Proton Pass agent token.
const PassTokenName = "PROTON_PASS_PERSONAL_ACCESS_TOKEN"

const refPrefix = "pass://"

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Info describes a secret without revealing it.
type Info struct {
	Name      string `json:"name"`
	Reference string `json:"reference,omitempty"` // the pass:// reference, if it is one
	Reserved  bool   `json:"reserved,omitempty"`
}

// Store reads and writes the .env registry.
type Store struct {
	path       string
	sessionDir string
	write      func([]byte) error

	mu       sync.Mutex
	lines    []string          // file content, comments and order preserved
	values   map[string]string // name -> raw value (literal or reference)
	resolved map[string]cached // reference -> value fetched from Proton Pass
	replacer *strings.Replacer
	loggedIn time.Time

	// Exec runs pass-cli; tests replace it.
	Exec func(ctx context.Context, env []string, args ...string) ([]byte, error)
}

type cached struct {
	value string
	at    time.Time
}

// Open loads the registry at path. write persists the file (the store's owner
// keeps the authoritative copy of locked files); sessionDir isolates the
// pass-cli session from the user's own.
func Open(path, sessionDir string, write func([]byte) error) (*Store, error) {
	s := &Store{path: path, sessionDir: sessionDir, write: write, resolved: map[string]cached{}}
	s.Exec = func(ctx context.Context, env []string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "pass-cli", args...)
		cmd.Env = append(os.Environ(), env...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("pass-cli %s: %s", args[0], strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	s.parse(string(b))
	return s, nil
}

func (s *Store) parse(text string) {
	s.lines, s.values = nil, map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		s.lines = append(s.lines, line)
		if name, value, ok := parseLine(line); ok {
			s.values[name] = value
		}
	}
	for len(s.lines) > 0 && s.lines[len(s.lines)-1] == "" {
		s.lines = s.lines[:len(s.lines)-1]
	}
	s.replacer = nil
}

func parseLine(line string) (name, value string, ok bool) {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	name, value, ok = strings.Cut(line, "=")
	name, value = strings.TrimSpace(name), strings.TrimSpace(value)
	if !ok || !namePattern.MatchString(name) {
		return "", "", false
	}
	if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
		value = value[1 : n-1]
	}
	return name, value, true
}

func isReserved(name string) bool {
	for _, r := range Reserved {
		if r == name {
			return true
		}
	}
	return false
}

// Names lists the registered secrets in file order.
func (s *Store) Names() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Info
	seen := map[string]bool{}
	for _, line := range s.lines {
		name, value, ok := parseLine(line)
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		info := Info{Name: name, Reserved: isReserved(name)}
		if strings.HasPrefix(value, refPrefix) {
			info.Reference = value
		}
		out = append(out, info)
	}
	return out
}

// Has reports whether name is registered (in the file or the process environment).
func (s *Store) Has(name string) bool {
	s.mu.Lock()
	_, ok := s.values[name]
	s.mu.Unlock()
	return ok || os.Getenv(name) != ""
}

// Set stores or replaces a secret. An empty value removes it.
func (s *Store) Set(name, value string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid secret name %q: use letters, digits and underscores", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("secret values must be a single line")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	replaced := false
	for _, line := range s.lines {
		if n, _, ok := parseLine(line); ok && n == name {
			if value != "" && !replaced {
				out = append(out, name+"="+quote(value))
				replaced = true
			}
			continue
		}
		out = append(out, line)
	}
	if value != "" && !replaced {
		out = append(out, name+"="+quote(value))
	}
	text := strings.Join(out, "\n")
	if text != "" {
		text += "\n"
	}
	if err := s.write([]byte(text)); err != nil {
		return err
	}
	s.parse(text)
	return nil
}

// Add stores a new secret on behalf of the agent: it refuses reserved names
// and never replaces an existing value.
func (s *Store) Add(name, value string) error {
	if isReserved(name) {
		return fmt.Errorf("%s is reserved for the harness", name)
	}
	if s.Has(name) {
		return fmt.Errorf("secret %s already exists; only the user can replace it", name)
	}
	if value == "" {
		return errors.New("empty value")
	}
	return s.Set(name, value)
}

func quote(value string) string {
	if strings.ContainsAny(value, " #'\"") && !strings.Contains(value, `"`) {
		return `"` + value + `"`
	}
	return value
}

// Get returns the value of a secret, resolving a Proton Pass reference if
// needed. reason is recorded in the Proton Pass audit log.
func (s *Store) Get(ctx context.Context, name, reason string) (string, error) {
	s.mu.Lock()
	raw, ok := s.values[name]
	s.mu.Unlock()
	if !ok {
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("no secret named %s", name)
	}
	if !strings.HasPrefix(raw, refPrefix) {
		return raw, nil
	}
	return s.resolve(ctx, raw, reason)
}

// Use returns a secret for something the agent asked for by name, such as
// filling a web form. Unlike Get it never hands out the harness's own
// credentials or anything from the process environment.
func (s *Store) Use(ctx context.Context, name, reason string) (string, error) {
	s.mu.Lock()
	_, ok := s.values[name]
	s.mu.Unlock()
	if isReserved(name) || !ok {
		return "", fmt.Errorf("no secret named %s", name)
	}
	return s.Get(ctx, name, reason)
}

// resolve fetches a pass:// reference, caching it briefly. One-time codes are
// never cached.
func (s *Store) resolve(ctx context.Context, ref, reason string) (string, error) {
	otp := strings.Contains(ref, "/totp")
	s.mu.Lock()
	c, hit := s.resolved[ref]
	s.mu.Unlock()
	if hit && !otp && time.Since(c.at) < 10*time.Minute {
		return c.value, nil
	}
	env, err := s.passEnv(ctx, reason)
	if err != nil {
		return "", err
	}
	out, err := s.Exec(ctx, env, "item", "view", ref)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("%s resolved to an empty value", ref)
	}
	s.mu.Lock()
	s.resolved[ref] = cached{value, time.Now()}
	s.replacer = nil
	s.mu.Unlock()
	return value, nil
}

// passEnv returns the environment for pass-cli and makes sure its session is
// logged in with the agent token.
func (s *Store) passEnv(ctx context.Context, reason string) ([]string, error) {
	s.mu.Lock()
	token := s.values[PassTokenName]
	fresh := time.Since(s.loggedIn) < 90*time.Minute // agent sessions last two hours
	s.mu.Unlock()
	if token == "" {
		token = os.Getenv(PassTokenName)
	}
	if token == "" {
		return nil, errors.New("Proton Pass is not set up: add the agent token in Settings")
	}
	if reason == "" {
		reason = "Artus"
	}
	if r := []rune(reason); len(r) > 300 {
		reason = string(r[:300])
	}
	env := []string{
		PassTokenName + "=" + token,
		"PROTON_PASS_SESSION_DIR=" + s.sessionDir,
		"PROTON_PASS_AGENT_REASON=" + reason,
	}
	if fresh {
		return env, nil
	}
	if err := os.MkdirAll(s.sessionDir, 0o700); err != nil {
		return nil, err
	}
	if _, err := s.Exec(ctx, env, "info"); err != nil {
		if _, err := s.Exec(ctx, env, "login"); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.loggedIn = time.Now()
	s.mu.Unlock()
	return env, nil
}

// PassEnv exposes the pass-cli environment so the agent can run pass-cli
// itself (for example to list a vault) inside its scoped session.
func (s *Store) PassEnv(ctx context.Context, reason string) ([]string, error) {
	return s.passEnv(ctx, reason)
}

var word = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// Env returns NAME=value pairs for every non-reserved secret that command
// mentions by name, plus the names used.
func (s *Store) Env(ctx context.Context, command, reason string) (env, used []string, err error) {
	seen := map[string]bool{}
	for _, name := range word.FindAllString(command, -1) {
		if seen[name] || isReserved(name) {
			continue
		}
		seen[name] = true
		s.mu.Lock()
		_, ok := s.values[name]
		s.mu.Unlock()
		if !ok {
			continue
		}
		value, err := s.Get(ctx, name, reason)
		if err != nil {
			return nil, nil, fmt.Errorf("secret %s: %w", name, err)
		}
		env = append(env, name+"="+value)
		used = append(used, name)
	}
	return env, used, nil
}

// Redact replaces every known secret value in text with a [secret:NAME] marker.
func (s *Store) Redact(text string) string {
	if text == "" {
		return text
	}
	s.mu.Lock()
	if s.replacer == nil {
		s.replacer = s.buildReplacer()
	}
	r := s.replacer
	s.mu.Unlock()
	return r.Replace(text)
}

func (s *Store) buildReplacer() *strings.Replacer {
	type pair struct{ value, name string }
	var pairs []pair
	add := func(name, value string) {
		if len(value) < 6 { // too short to be a secret worth mangling output for
			return
		}
		pairs = append(pairs, pair{value, name})
		if enc := url.QueryEscape(value); enc != value {
			pairs = append(pairs, pair{enc, name})
		}
		pairs = append(pairs, pair{base64.StdEncoding.EncodeToString([]byte(value)), name})
	}
	for name, value := range s.values {
		if ref := value; strings.HasPrefix(ref, refPrefix) {
			if c, ok := s.resolved[ref]; ok {
				add(name, c.value)
			}
			continue
		}
		add(name, value)
	}
	for _, name := range Reserved {
		if _, ok := s.values[name]; !ok {
			add(name, os.Getenv(name))
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].value) > len(pairs[j].value) })
	args := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		args = append(args, p.value, "[secret:"+p.name+"]")
	}
	return strings.NewReplacer(args...)
}

// Masked renders the registry for the model: names and references stay
// visible, literal values do not.
func (s *Store) Masked() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, line := range s.lines {
		if name, value, ok := parseLine(line); ok {
			if !strings.HasPrefix(value, refPrefix) {
				value = "********"
			}
			line = name + "=" + value
		}
		b.WriteString(line + "\n")
	}
	if b.Len() == 0 {
		return "(no secrets stored)\n"
	}
	return b.String()
}
