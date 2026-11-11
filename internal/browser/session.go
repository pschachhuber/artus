package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// session is one named browser: a process (or an attached external browser),
// its DevTools connection and its tabs.
type session struct {
	m    *Manager
	name string

	headless    bool
	external    bool // attached through cdpEndpoint; never shut down by us
	profileDir  string
	tempProfile bool
	conn        *cdpConn
	root        *cdpSession
	proc        *browserProc

	// cmdMu serialises commands. A hand-off releases it while waiting for
	// the user.
	cmdMu sync.Mutex

	// mu guards everything below, including the state of all tabs and
	// frames. changed is closed and replaced whenever that state changes.
	mu         sync.Mutex
	cfg        config
	changed    chan struct{}
	tabs       []*tab
	current    *tab
	targets    map[string]*target // by protocol session id
	helpers    int                // helper tabs about to attach
	helperTabs []*tab             // internal tabs, hidden from the tab list
	opened     []*tab             // tabs opened since the last response
	downloads  map[string]*download
	notes      []note   // events to mention in the next response
	files      []string // files produced by events since the last response
	handoff    *handoff
	snapSeq    int
	viewport   *viewport // emulated viewport, applied to every tab
	closing    bool

	downloadActivity time.Time // last download event

	// Live view: who is watching, which tab is being cast, and the pointer
	// state of the user driving it.
	viewers   map[int]chan []byte
	viewerSeq int
	casting   *tab
	lastFrame []byte
	live      liveInput

	// castMu serialises starting and stopping the screencast.
	castMu sync.Mutex

	// Fixed at launch.
	initScripts []string
	filter      originFilter
}

// acquire returns the session a command addresses, starting its browser when
// it is not running. `open` with launch flags that contradict the running
// browser restarts it.
func (m *Manager) acquire(ctx context.Context, c *command) (*session, error) {
	inv := c.inv
	if s := m.lookup(inv.session); s != nil && !(inv.cmd == "open" && s.launchMismatch(c)) {
		return s, nil
	}
	m.launchMu.Lock()
	defer m.launchMu.Unlock()
	if s := m.lookup(inv.session); s != nil {
		if !(inv.cmd == "open" && s.launchMismatch(c)) {
			return s, nil
		}
		s.close()
		c.out.say("Restarted the browser with the requested options.")
	}
	s, err := m.openSession(ctx, c)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sessions[s.name] = s
	m.mu.Unlock()
	m.emit(Event{Type: "session_open", Session: s.name})
	return s, nil
}

// launchMismatch reports whether explicit launch flags differ from how the
// running browser was started.
func (s *session) launchMismatch(c *command) bool {
	inv := c.inv
	if s.external {
		return false
	}
	if inv.has("headed") && s.headless || inv.has("headless") && !s.headless {
		return true
	}
	if inv.has("isolated") != s.tempProfile && (inv.has("isolated") || inv.has("profile")) {
		return true
	}
	if p, ok := inv.flags["profile"]; ok && c.resolvePath(p) != s.profileDir {
		return true
	}
	return false
}

func (m *Manager) openSession(ctx context.Context, c *command) (*session, error) {
	cfg, inv := c.cfg, c.inv
	s := &session{
		m: m, name: inv.session, cfg: cfg,
		changed:   make(chan struct{}),
		targets:   map[string]*target{},
		downloads: map[string]*download{},
		viewers:   map[int]chan []byte{},
		filter:    newOriginFilter(cfg.Network),
	}
	for _, file := range cfg.InitScripts {
		if !filepath.IsAbs(file) {
			file = filepath.Join(m.opts.Dir, file)
		}
		source, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("initScripts in %s: %w", configPath(m.opts.Dir), err)
		}
		s.initScripts = append(s.initScripts, string(source))
	}
	var endpoint string
	var err error
	if cfg.CDPEndpoint != "" {
		s.external = true
		if endpoint, err = resolveCDPEndpoint(ctx, cfg.CDPEndpoint); err != nil {
			return nil, err
		}
	} else if endpoint, err = s.launch(ctx, c); err != nil {
		return nil, err
	}
	fail := func(err error) (*session, error) {
		if s.proc != nil {
			s.proc.kill()
		}
		s.removeTempProfile()
		return nil, err
	}
	if s.conn, err = dialCDP(ctx, endpoint); err != nil {
		return fail(fmt.Errorf("connect to browser: %w", err))
	}
	if err := s.start(ctx); err != nil {
		s.conn.Close()
		return fail(err)
	}
	go func() {
		<-s.conn.done
		s.close()
	}()
	return s, nil
}

// launch starts a browser process for the session and returns its DevTools
// endpoint.
func (s *session) launch(ctx context.Context, c *command) (string, error) {
	cfg, inv, dir := c.cfg, c.inv, s.m.opts.Dir
	switch {
	case inv.flags["profile"] != "":
		s.profileDir = c.resolvePath(inv.flags["profile"])
	case inv.has("isolated"):
		tmp, err := os.MkdirTemp("", "artus-browser-")
		if err != nil {
			return "", err
		}
		s.profileDir, s.tempProfile = tmp, true
	default:
		s.profileDir = filepath.Join(dir, "profiles", s.name)
	}
	switch {
	case inv.has("headed"):
		s.headless = false
	case inv.has("headless"):
		s.headless = true
	case cfg.Headless == headlessAuto:
		s.headless = !displayAvailable()
	default:
		s.headless = cfg.Headless == headlessOn
	}
	found, _, err := s.m.resolveBrowser(ctx, cfg, true)
	if err != nil {
		s.removeTempProfile()
		return "", err
	}
	spec := launchSpec{
		exe: found.path, userDataDir: s.profileDir, headless: s.headless,
		noSandbox: cfg.NoSandbox || runningAsRoot(), cfg: cfg,
	}
	reaped := s.reapStale(ctx)
	proc, endpoint, err := launchBrowser(ctx, spec)
	if err != nil && reaped && ctx.Err() == nil {
		// The previous owner of the profile may still be shutting down.
		time.Sleep(time.Second)
		proc, endpoint, err = launchBrowser(ctx, spec)
	}
	if err != nil {
		s.removeTempProfile()
		return "", err
	}
	s.proc = proc
	if s.headless {
		// A headless window's content area is smaller than its window size;
		// emulate the configured viewport exactly.
		s.viewport = &cfg.Viewport
	}
	return endpoint, nil
}

// reapStale shuts down a browser left running on the profile by an earlier
// Artus process; a second instance on the same profile would not start.
func (s *session) reapStale(ctx context.Context) bool {
	endpoint, ok := readDevToolsActivePort(filepath.Join(s.profileDir, "DevToolsActivePort"))
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := dialCDP(ctx, endpoint)
	if err != nil {
		return false
	}
	defer conn.Close()
	s.m.logf("browser: closing a stale browser on profile %s", s.profileDir)
	conn.session("", nil).call(ctx, "Browser.close", nil, nil)
	select {
	case <-conn.done:
	case <-ctx.Done():
	}
	return true
}

// start wires up the browser-level protocol session and waits for a usable
// tab.
func (s *session) start(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.root = s.conn.session("", s.onBrowserEvent)
	downloads := s.cfg.downloadsDir(s.m.opts.Dir)
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return err
	}
	err := s.root.call(ctx, "Browser.setDownloadBehavior", obj{
		"behavior": "allowAndName", "downloadPath": downloads, "eventsEnabled": true,
	}, nil)
	if err != nil {
		s.m.logf("browser: downloads are not tracked: %v", err)
	}
	err = s.root.call(ctx, "Target.setAutoAttach", obj{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
	}, nil)
	if err != nil {
		return err
	}
	// A freshly launched browser opens its first tab on its own; give it a
	// moment before creating one ourselves.
	grace, stop := context.WithTimeout(ctx, 3*time.Second)
	err = s.wait(grace, func() bool { return len(s.tabs) > 0 })
	stop()
	if err != nil {
		if _, err := s.newTab(ctx, "about:blank"); err != nil {
			return err
		}
	}
	if _, err := s.currentTab(ctx); err != nil {
		return fmt.Errorf("browser tab did not become ready: %w", err)
	}
	// Tabs that were already open are not news.
	s.mu.Lock()
	s.opened = nil
	s.mu.Unlock()
	return nil
}

// dead reports whether the browser connection is gone.
func (s *session) dead() bool {
	select {
	case <-s.conn.done:
		return true
	default:
		return false
	}
}

// close shuts the session down. It is safe to call more than once and from
// any goroutine.
func (s *session) close() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	h := s.handoff
	s.notify()
	s.mu.Unlock()
	if h != nil {
		h.finish("closed")
	}
	if !s.external {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		s.root.call(ctx, "Browser.close", nil, nil)
		cancel()
		if s.proc != nil && !s.proc.waitExit(5*time.Second) {
			s.proc.kill()
		}
	}
	s.conn.Close()
	s.removeTempProfile()
	s.m.mu.Lock()
	if s.m.sessions[s.name] == s {
		delete(s.m.sessions, s.name)
	}
	s.m.mu.Unlock()
	s.m.emit(Event{Type: "session_close", Session: s.name})
}

func (s *session) removeTempProfile() {
	if !s.tempProfile {
		return
	}
	// Child processes can outlive the browser process by a moment and still
	// be writing into the profile.
	for i := 0; i < 20; i++ {
		if os.RemoveAll(s.profileDir) == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// refreshConfig applies the parts of the config that can change while the
// browser runs.
func (s *session) refreshConfig(cfg config) {
	s.mu.Lock()
	s.cfg.Timeouts, s.cfg.Snapshot = cfg.Timeouts, cfg.Snapshot
	s.mu.Unlock()
}

// notify wakes everything waiting on session state. Callers hold s.mu.
func (s *session) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// wait blocks until cond, evaluated with s.mu held, reports true.
func (s *session) wait(ctx context.Context, cond func() bool) error {
	for {
		s.mu.Lock()
		ok, changed := cond(), s.changed
		s.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.conn.done:
			return errConnClosed
		}
	}
}

func (s *session) status() SessionStatus {
	st := SessionStatus{Name: s.name, Headless: s.headless}
	s.mu.Lock()
	st.Tabs = len(s.tabs)
	t := s.current
	if s.handoff != nil {
		ev := s.handoff.event
		st.Handoff = &ev
	}
	s.mu.Unlock()
	if t != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		st.URL, st.Title = t.urlAndTitle(ctx)
	}
	return st
}

// timeouts returns the current action and navigation timeouts.
func (s *session) timeouts() (action, navigation time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.actionTimeout(), s.cfg.navigationTimeout()
}

// describeMode names how the session's browser is shown.
func (s *session) describeMode() string {
	switch {
	case s.external:
		return "attached via cdpEndpoint"
	case s.headless:
		return "headless"
	}
	return "headed"
}

// profileLabel describes where the session keeps its state.
func (s *session) profileLabel() string {
	switch {
	case s.external:
		return "external browser"
	case s.tempProfile:
		return "isolated (temporary profile)"
	}
	return s.profileDir
}

var errNoTab = errors.New("the tab was closed")
