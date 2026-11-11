package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// installedBrowser is a Chromium-family browser found on this machine.
type installedBrowser struct {
	kind string // chrome, edge, brave, chromium or chrome-for-testing
	path string
}

// browserKinds is the order in which installed browsers are preferred.
var browserKinds = []string{"chrome", "edge", "brave", "chromium"}

// browserLocations lists, per browser kind and OS, executable names to look
// up on PATH (no separator) and well-known install paths. On Windows the
// paths are relative to the Program Files and LocalAppData directories; on
// macOS a path is also tried under the user's home.
var browserLocations = map[string]map[string][]string{
	"linux": {
		"chrome":   {"google-chrome-stable", "google-chrome", "/opt/google/chrome/chrome"},
		"edge":     {"microsoft-edge-stable", "microsoft-edge", "/opt/microsoft/msedge/msedge"},
		"brave":    {"brave-browser", "brave", "/opt/brave.com/brave/brave"},
		"chromium": {"chromium", "chromium-browser"},
	},
	"darwin": {
		"chrome":   {"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"},
		"edge":     {"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
		"brave":    {"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"},
		"chromium": {"/Applications/Chromium.app/Contents/MacOS/Chromium"},
	},
	"windows": {
		"chrome":   {`Google\Chrome\Application\chrome.exe`},
		"edge":     {`Microsoft\Edge\Application\msedge.exe`},
		"brave":    {`BraveSoftware\Brave-Browser\Application\brave.exe`},
		"chromium": {`Chromium\Application\chrome.exe`},
	},
}

// findInstalled looks for a browser of the given kind at its standard
// locations.
func findInstalled(kind string) (string, bool) {
	for _, candidate := range browserLocations[runtime.GOOS][kind] {
		var paths []string
		switch {
		case runtime.GOOS == "windows":
			for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
				if base := os.Getenv(env); base != "" {
					paths = append(paths, filepath.Join(base, candidate))
				}
			}
		case !strings.ContainsRune(candidate, '/'):
			if p, err := exec.LookPath(candidate); err == nil {
				paths = append(paths, p)
			}
		default:
			paths = append(paths, candidate)
			if home, err := os.UserHomeDir(); err == nil && runtime.GOOS == "darwin" {
				paths = append(paths, filepath.Join(home, candidate))
			}
		}
		for _, p := range paths {
			if usableExecutable(p) {
				return p, true
			}
		}
	}
	return "", false
}

// usableExecutable reports whether path is a browser binary we can drive.
// Snap packages are skipped: their confinement cannot use a profile
// directory outside ~/snap.
func usableExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && strings.HasPrefix(resolved, "/snap/") {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode()&0o111 != 0
}

// resolveBrowser picks the browser executable, in order: the configured
// executablePath, an installed browser, a previously downloaded Chrome for
// Testing, and finally (when download is set) a fresh download of it.
func (m *Manager) resolveBrowser(ctx context.Context, cfg config, download bool) (installedBrowser, string, error) {
	if cfg.ExecutablePath != "" {
		if _, err := os.Stat(cfg.ExecutablePath); err != nil {
			return installedBrowser{}, "", fmt.Errorf("executablePath in %s: %w", configPath(m.opts.Dir), err)
		}
		return installedBrowser{kind: "custom", path: cfg.ExecutablePath}, "executablePath in the configuration", nil
	}
	kinds := browserKinds
	switch cfg.Browser {
	case "auto":
	case "chrome-for-testing":
		kinds = nil
	default:
		kinds = []string{cfg.Browser}
	}
	for _, kind := range kinds {
		if p, ok := findInstalled(kind); ok {
			return installedBrowser{kind: kind, path: p}, "installed on this machine", nil
		}
	}
	if cfg.Browser != "auto" && cfg.Browser != "chrome-for-testing" {
		return installedBrowser{}, "", fmt.Errorf("browser %q is not installed at its usual location; install it, set executablePath, or set browser to \"auto\" in %s", cfg.Browser, configPath(m.opts.Dir))
	}
	if rec, err := readInstallRecord(m.opts.Dir); err == nil {
		return installedBrowser{kind: "chrome-for-testing", path: rec.executable(m.opts.Dir)}, "downloaded Chrome for Testing " + rec.Version, nil
	}
	if !download {
		return installedBrowser{}, "", errors.New("no installed browser found and Chrome for Testing is not downloaded yet")
	}
	rec, _, err := m.installChrome(ctx, false)
	if err != nil {
		return installedBrowser{}, "", fmt.Errorf("no Chromium-family browser is installed and downloading Chrome for Testing failed: %w", err)
	}
	return installedBrowser{kind: "chrome-for-testing", path: rec.executable(m.opts.Dir)}, "downloaded Chrome for Testing " + rec.Version, nil
}

// installRecord describes the Chrome for Testing build under <dir>/chrome.
type installRecord struct {
	Version    string `json:"version"`
	Platform   string `json:"platform"`
	Executable string `json:"executable"` // relative to <dir>/chrome
}

func chromeDir(dir string) string { return filepath.Join(dir, "chrome") }

func (r installRecord) executable(dir string) string {
	return filepath.Join(chromeDir(dir), filepath.FromSlash(r.Executable))
}

// readInstallRecord returns the recorded download if its executable still
// exists.
func readInstallRecord(dir string) (installRecord, error) {
	var rec installRecord
	data, err := os.ReadFile(filepath.Join(chromeDir(dir), "installed.json"))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	if _, err := os.Stat(rec.executable(dir)); err != nil {
		return rec, err
	}
	return rec, nil
}

// browserVersion asks an executable for its version string.
func browserVersion(ctx context.Context, exe string) string {
	if runtime.GOOS == "windows" {
		// Chrome on Windows does not print its version; the directory next
		// to the executable is named after it.
		entries, _ := os.ReadDir(filepath.Dir(exe))
		for _, e := range entries {
			if e.IsDir() && strings.Count(e.Name(), ".") == 3 {
				return e.Name()
			}
		}
		return "unknown"
	}
	out, err := exec.CommandContext(ctx, exe, "--version").Output()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	return strings.TrimSpace(string(out))
}
