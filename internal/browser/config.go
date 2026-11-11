package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// config is the content of <dir>/config.json. The file is re-read for every
// command, so edits apply immediately; launch settings take effect the next
// time a session's browser starts.
type config struct {
	Browser        string         `json:"browser"`
	ExecutablePath string         `json:"executablePath"`
	Headless       headlessMode   `json:"headless"`
	Viewport       viewport       `json:"viewport"`
	Locale         string         `json:"locale"`
	Timezone       string         `json:"timezone"`
	Proxy          proxyConfig    `json:"proxy"`
	Args           []string       `json:"args"`
	NoSandbox      bool           `json:"noSandbox"`
	CDPEndpoint    string         `json:"cdpEndpoint"`
	Timeouts       timeoutConfig  `json:"timeouts"`
	Network        networkConfig  `json:"network"`
	Snapshot       snapshotConfig `json:"snapshot"`
	DownloadsDir   string         `json:"downloadsDir"`
	InitScripts    []string       `json:"initScripts"`
}

type viewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type proxyConfig struct {
	Server string `json:"server"`
	Bypass string `json:"bypass"`
}

// timeoutConfig holds timeouts in milliseconds.
type timeoutConfig struct {
	Action     int `json:"action"`
	Navigation int `json:"navigation"`
	Handoff    int `json:"handoff"`
}

type networkConfig struct {
	AllowedOrigins []string `json:"allowedOrigins"`
	BlockedOrigins []string `json:"blockedOrigins"`
}

type snapshotConfig struct {
	InlineMaxChars int `json:"inlineMaxChars"`
}

// headlessMode is "auto", "on" or "off"; in JSON it is "auto", true or false.
type headlessMode string

const (
	headlessAuto headlessMode = "auto"
	headlessOn   headlessMode = "on"
	headlessOff  headlessMode = "off"
)

func (h headlessMode) MarshalJSON() ([]byte, error) {
	switch h {
	case headlessOn:
		return []byte("true"), nil
	case headlessOff:
		return []byte("false"), nil
	}
	return []byte(`"auto"`), nil
}

func (h *headlessMode) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true", `"true"`:
		*h = headlessOn
	case "false", `"false"`:
		*h = headlessOff
	case `"auto"`, `""`, "null":
		*h = headlessAuto
	default:
		return fmt.Errorf(`headless must be "auto", true or false, not %s`, data)
	}
	return nil
}

func defaultConfig() config {
	return config{
		Browser:  "auto",
		Headless: headlessAuto,
		Viewport: viewport{Width: 1280, Height: 720},
		Args:     []string{},
		Timeouts: timeoutConfig{Action: 5000, Navigation: 60000, Handoff: 600000},
		Network:  networkConfig{AllowedOrigins: []string{}, BlockedOrigins: []string{}},
		Snapshot: snapshotConfig{InlineMaxChars: 6000},

		InitScripts: []string{},
	}
}

func configPath(dir string) string { return filepath.Join(dir, "config.json") }

// loadConfig reads <dir>/config.json over the defaults, so missing keys keep
// their default values. A missing file is created with the defaults.
func loadConfig(dir string) (config, error) {
	cfg := defaultConfig()
	path := configPath(dir)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, writeConfig(dir, cfg)
	}
	if err != nil {
		return cfg, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("%s is not valid: %w (fix the file or delete it to restore defaults)", path, err)
		}
	}
	def := defaultConfig()
	if cfg.Browser == "" {
		cfg.Browser = def.Browser
	}
	if cfg.Viewport.Width <= 0 || cfg.Viewport.Height <= 0 {
		cfg.Viewport = def.Viewport
	}
	if cfg.Timeouts.Action <= 0 {
		cfg.Timeouts.Action = def.Timeouts.Action
	}
	if cfg.Timeouts.Navigation <= 0 {
		cfg.Timeouts.Navigation = def.Timeouts.Navigation
	}
	if cfg.Timeouts.Handoff <= 0 {
		cfg.Timeouts.Handoff = def.Timeouts.Handoff
	}
	if cfg.Snapshot.InlineMaxChars <= 0 {
		cfg.Snapshot.InlineMaxChars = def.Snapshot.InlineMaxChars
	}
	switch cfg.Browser {
	case "auto", "chrome", "edge", "brave", "chromium", "chrome-for-testing":
	default:
		return cfg, fmt.Errorf(`%s: browser must be one of auto, chrome, edge, brave, chromium, chrome-for-testing, not %q`, path, cfg.Browser)
	}
	return cfg, nil
}

func writeConfig(dir string, cfg config) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(dir), append(data, '\n'), 0o644)
}

func (c config) actionTimeout() time.Duration {
	return time.Duration(c.Timeouts.Action) * time.Millisecond
}

func (c config) navigationTimeout() time.Duration {
	return time.Duration(c.Timeouts.Navigation) * time.Millisecond
}

func (c config) handoffTimeout() time.Duration {
	return time.Duration(c.Timeouts.Handoff) * time.Millisecond
}

// downloadsDir returns the directory finished downloads are moved to.
func (c config) downloadsDir(dir string) string {
	if c.DownloadsDir != "" {
		return c.DownloadsDir
	}
	return filepath.Join(dir, "downloads")
}

func cmdConfig(c *command) error {
	data, err := json.MarshalIndent(c.cfg, "", "  ")
	if err != nil {
		return err
	}
	c.out.say("Configuration file: %s", c.show(configPath(c.m.opts.Dir)))
	c.out.say("Edit the file to change settings. Launch settings (browser, headless, proxy, args, network rules, init scripts) apply the next time a session's browser starts.")
	c.out.say("%s", data)
	return nil
}
