package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// disabledFeatures turns off browser features that only get in the way of an
// unattended browser: background network chatter, translation bubbles and
// first-run surfaces. None of them alter how pages see the browser.
var disabledFeatures = []string{
	"Translate",
	"MediaRouter",
	"OptimizationHints",
	"DialMediaRouteProvider",
	"AutofillServerCommunication",
	"CertificateTransparencyComponentUpdater",
	"PrivacySandboxSettings4",
	"msForceBrowserSignIn",
}

type launchSpec struct {
	exe         string
	userDataDir string
	headless    bool
	noSandbox   bool
	cfg         config
}

// args returns the command line for a browser we own.
func (l launchSpec) args() []string {
	args := []string{
		"--remote-debugging-port=0",
		"--user-data-dir=" + l.userDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--no-service-autorun",
		"--disable-back-forward-cache",
		"--disable-background-networking",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-breakpad",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-hang-monitor",
		"--disable-popup-blocking",
		"--disable-prompt-on-repost",
		"--disable-search-engine-choice-screen",
		"--disable-smooth-scrolling",
		"--disable-sync",
		"--disable-infobars",
		"--metrics-recording-only",
		"--password-store=basic",
		"--use-mock-keychain",
		"--disable-features=" + strings.Join(disabledFeatures, ","),
		fmt.Sprintf("--window-size=%d,%d", l.cfg.Viewport.Width, l.cfg.Viewport.Height),
	}
	if runtime.GOOS == "linux" {
		args = append(args, "--disable-dev-shm-usage")
	}
	if l.headless {
		args = append(args, "--headless=new", "--hide-scrollbars", "--mute-audio")
	}
	if l.noSandbox {
		args = append(args, "--no-sandbox")
	}
	if l.cfg.Locale != "" {
		args = append(args, "--lang="+l.cfg.Locale, "--accept-lang="+l.cfg.Locale)
	}
	if l.cfg.Proxy.Server != "" {
		args = append(args, "--proxy-server="+l.cfg.Proxy.Server)
		if l.cfg.Proxy.Bypass != "" {
			args = append(args, "--proxy-bypass-list="+l.cfg.Proxy.Bypass)
		}
	}
	args = append(args, l.cfg.Args...)
	return append(args, "about:blank")
}

// browserProc is a browser process started by us.
type browserProc struct {
	cmd    *exec.Cmd
	exited chan struct{}
	stderr tailBuffer
}

// launchBrowser starts the browser and returns once its DevTools endpoint is
// known.
func launchBrowser(ctx context.Context, spec launchSpec) (*browserProc, string, error) {
	if err := os.MkdirAll(spec.userDataDir, 0o700); err != nil {
		return nil, "", err
	}
	portFile := filepath.Join(spec.userDataDir, "DevToolsActivePort")
	os.Remove(portFile)

	p := &browserProc{exited: make(chan struct{})}
	p.cmd = exec.Command(spec.exe, spec.args()...)
	p.cmd.Stderr = &p.stderr
	isolateProcess(p.cmd)
	if err := p.cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("start %s: %w", spec.exe, err)
	}
	go func() {
		p.cmd.Wait()
		close(p.exited)
	}()

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if endpoint, ok := readDevToolsActivePort(portFile); ok {
			return p, endpoint, nil
		}
		select {
		case <-p.exited:
			return nil, "", fmt.Errorf("browser exited during startup: %s\nRun `artus browser doctor` to check for missing system libraries.", p.stderr.tail())
		case <-ctx.Done():
			p.kill()
			return nil, "", fmt.Errorf("browser did not open a DevTools endpoint: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// readDevToolsActivePort parses the file Chromium writes once its debugging
// server is listening: the port on the first line, the browser target path on
// the second.
func readDevToolsActivePort(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return "", false
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || port <= 0 {
		return "", false
	}
	return fmt.Sprintf("ws://127.0.0.1:%d%s", port, strings.TrimSpace(lines[1])), true
}

// kill terminates the process tree and waits for it to be reaped.
func (p *browserProc) kill() {
	select {
	case <-p.exited:
		return
	default:
	}
	killProcessTree(p.cmd)
	<-p.exited
}

// waitExit waits up to d for the process to exit on its own.
func (p *browserProc) waitExit(d time.Duration) bool {
	select {
	case <-p.exited:
		return true
	case <-time.After(d):
		return false
	}
}

// tailBuffer keeps the last few kilobytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	const limit = 4 << 10
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > limit {
		t.buf = t.buf[len(t.buf)-limit:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuffer) tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// displayAvailable reports whether a headed browser window can be shown.
func displayAvailable() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// resolveCDPEndpoint turns a configured endpoint into a browser WebSocket
// URL. It accepts ws(s):// URLs as is and asks http(s):// endpoints for
// /json/version, as `chrome --remote-debugging-port` serves it.
func resolveCDPEndpoint(ctx context.Context, endpoint string) (string, error) {
	if strings.HasPrefix(endpoint, "ws://") || strings.HasPrefix(endpoint, "wss://") {
		return endpoint, nil
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	var info struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := httpGetJSON(ctx, strings.TrimRight(endpoint, "/")+"/json/version", &info); err != nil {
		return "", fmt.Errorf("cdpEndpoint %s: %w", endpoint, err)
	}
	if info.WebSocketDebuggerURL == "" {
		return "", errors.New("cdpEndpoint " + endpoint + " did not report a webSocketDebuggerUrl")
	}
	return info.WebSocketDebuggerURL, nil
}
