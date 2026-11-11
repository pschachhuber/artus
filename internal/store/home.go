// Package store owns the Artus data home: its layout, configuration, chat
// logs and the change history of everything the agent is allowed to edit.
package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Home is an opened data directory (default ~/.artus).
type Home struct {
	Dir string

	mu    sync.Mutex
	cfg   Config
	guard map[string][]byte // locked files as the harness last wrote them
}

// Locked files and directories, relative to the home. The agent may not write
// them; only the user (through Settings) and the harness itself do.
const (
	ConfigFile       = "config.json"
	SecretsFile      = ".env"
	InstructionsFile = "instructions.md"
	ScheduleFile     = "schedule.json"
	MemoryFile       = "memory.md"
)

var lockedFiles = []string{ConfigFile, SecretsFile, InstructionsFile, ScheduleFile}
var lockedDirs = []string{"chats", ".git", "pass-session"}

// DefaultDir returns $ARTUS_HOME or ~/.artus.
func DefaultDir() string {
	if d := os.Getenv("ARTUS_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".artus"
	}
	return filepath.Join(home, ".artus")
}

// Open prepares the directory layout, seeds missing defaults from seed and
// loads the configuration.
func Open(dir string, seed fs.FS) (*Home, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	h := &Home{Dir: abs, guard: map[string][]byte{}}
	for _, d := range []string{"", "topics", "skills", "tools", "tasks", "chats", "uploads", "tmp", "browser"} {
		if err := os.MkdirAll(filepath.Join(abs, d), 0o700); err != nil {
			return nil, err
		}
	}
	if seed != nil {
		if err := h.seed(seed); err != nil {
			return nil, err
		}
	}
	if err := h.loadConfig(); err != nil {
		return nil, err
	}
	for _, f := range lockedFiles {
		if b, err := os.ReadFile(h.Path(f)); err == nil {
			h.guard[f] = b
		}
	}
	h.initHistory()
	return h, nil
}

// seed copies files from the embedded defaults that do not exist yet, so user
// and agent edits are never overwritten.
func (h *Home) seed(seed fs.FS) error {
	return fs.WalkDir(seed, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		dst := h.Path(filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
		b, err := fs.ReadFile(seed, p)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o600)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o700
		}
		return os.WriteFile(dst, b, mode)
	})
}

// Path joins parts onto the home directory.
func (h *Home) Path(parts ...string) string {
	return filepath.Join(append([]string{h.Dir}, parts...)...)
}

// Rel returns p relative to the home using forward slashes, and whether p is
// inside the home at all.
func (h *Home) Rel(p string) (string, bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	root := h.Dir
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// Locked reports whether the agent is forbidden to write path p.
func (h *Home) Locked(p string) bool {
	rel, ok := h.Rel(p)
	if !ok {
		return false
	}
	for _, f := range lockedFiles {
		if strings.EqualFold(rel, f) {
			return true
		}
	}
	for _, d := range lockedDirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// WriteLocked writes a locked file on behalf of the user or the harness and
// records the content as the authoritative copy.
func (h *Home) WriteLocked(rel string, data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.writeLocked(rel, data)
}

func (h *Home) writeLocked(rel string, data []byte) error {
	if err := writeAtomic(h.Path(rel), data, 0o600); err != nil {
		return err
	}
	h.guard[rel] = append([]byte(nil), data...)
	return nil
}

// Restore puts back any locked file that was changed behind the harness's
// back (for example by a shell command) and returns the names it restored.
func (h *Home) Restore() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var restored []string
	for rel, want := range h.guard {
		got, err := os.ReadFile(h.Path(rel))
		if err == nil && bytes.Equal(got, want) {
			continue
		}
		if writeAtomic(h.Path(rel), want, 0o600) == nil {
			restored = append(restored, rel)
		}
	}
	return restored
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Config is the user-owned configuration in config.json.
type Config struct {
	Model         string   `json:"model"`  // "provider/model-id"
	Effort        string   `json:"effort"` // low | medium | high | xhigh | max
	AllowedModels []string `json:"allowedModels"`
	Autonomy      string   `json:"autonomy"` // auto | confirm
	WebSearch     bool     `json:"webSearch"`
	Limits        Limits   `json:"limits"`
	Context       Context  `json:"context"`
	Server        Server   `json:"server"`
	Secrets       Secrets  `json:"secrets"`
}

// Limits bound what a single run may consume.
type Limits struct {
	Subagents  int   `json:"subagents"`  // sub-agents running at once
	Depth      int   `json:"depth"`      // how deep sub-agents may nest
	Turns      int   `json:"turns"`      // model requests per run
	TaskTokens int64 `json:"taskTokens"` // token budget of one unattended task run
}

// Context controls compaction and tool-output handling.
type Context struct {
	CompactAt        float64 `json:"compactAt"`        // fraction of the model's window
	CompactMaxTokens int     `json:"compactMaxTokens"` // absolute cap on the trigger
	ToolOutputChars  int     `json:"toolOutputChars"`  // per tool result kept in context
	DefaultWindow    int     `json:"defaultWindow"`    // window assumed for unknown models
}

// Server holds the local web server settings.
type Server struct {
	Host  string `json:"host"`
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// Secrets selects where secret values come from.
type Secrets struct {
	Backend string `json:"backend"` // env | protonpass
	Vault   string `json:"vault"`   // Proton Pass vault the agent token is scoped to
}

func defaultConfig() Config {
	return Config{
		Effort:        "medium",
		AllowedModels: []string{},
		Autonomy:      "auto",
		WebSearch:     true,
		Limits:        Limits{Subagents: 8, Depth: 1, Turns: 150, TaskTokens: 3_000_000},
		Context:       Context{CompactAt: 0.7, CompactMaxTokens: 200_000, ToolOutputChars: 30_000, DefaultWindow: 200_000},
		Server:        Server{Host: "127.0.0.1", Port: 7717},
		Secrets:       Secrets{Backend: "env"},
	}
}

func (h *Home) loadConfig() error {
	cfg := defaultConfig()
	b, err := os.ReadFile(h.Path(ConfigFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("%s: %w", ConfigFile, err)
		}
	}
	if cfg.Server.Token == "" {
		cfg.Server.Token = RandomID(24)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
	return h.saveConfig()
}

func (h *Home) saveConfig() error {
	b, err := json.MarshalIndent(h.cfg, "", "  ")
	if err != nil {
		return err
	}
	return h.writeLocked(ConfigFile, append(b, '\n'))
}

// Config returns a copy of the current configuration.
func (h *Home) Config() Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.cfg
	c.AllowedModels = append([]string(nil), h.cfg.AllowedModels...)
	return c
}

// UpdateConfig applies fn to the configuration and saves it.
func (h *Home) UpdateConfig(fn func(*Config)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(&h.cfg)
	if h.cfg.AllowedModels == nil {
		h.cfg.AllowedModels = []string{}
	}
	return h.saveConfig()
}

// RandomID returns n random bytes as hex.
func RandomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
