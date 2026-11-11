package browser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEvalFindWaitConsoleNetwork(t *testing.T) {
	e := shared(t)
	e.run("goto", e.site.url("/console"))
	mustContain(t, e.run("eval", "1 + 2"), "### Result\n3")
	mustContain(t, e.run("eval", "() => ({title: document.title, n: [1, 2]})"), `"title": "Console"`, `"n": [`)
	mustContain(t, e.run("eval", "async () => { await new Promise(r => setTimeout(r, 20)); return 'later'; }"), `"later"`)
	mustContain(t, e.run("eval", "const a = 20; a * 2"), "40") // statements, not an expression
	mustContain(t, e.run("eval", "document.body.dataset.x"), "undefined")
	mustContain(t, e.run("eval", "el => el.tagName", "p"), `"P"`)
	mustContain(t, e.fail("eval", "nope.nope"), "ReferenceError: nope is not defined")
	mustContain(t, e.fail("eval", "el => el.missing.x", "p"), "TypeError")
	mustContain(t, e.fail("eval", "new Promise(() => {})", "--timeout", "300"), "did not finish within 300ms")

	out := e.run("console")
	mustContain(t, out, "[INFO] hello log", "[WARNING] careful", "[ERROR] boom {code: 7}", "[ERROR] Error: uncaught failure")
	errorsOnly := e.run("console", "--level", "error")
	mustContain(t, errorsOnly, "[ERROR] boom")
	mustNotContain(t, errorsOnly, "hello log", "careful")
	mustContain(t, e.fail("console", "--level", "loud"), "the level must be one of")

	mustContain(t, e.run("network"), "[GET] "+e.site.url("/console")+" => [200]")
	// A failed document load is an error, and a 404 is flagged in the header.
	mustContain(t, e.run("goto", e.site.url("/no-such-page")), "- HTTP status: 404")
	mustContain(t, e.fail("goto", "http://127.0.0.1:1/"), "navigation to http://127.0.0.1:1/ failed: net::ERR_")

	// New console errors are announced with the next page report.
	out = e.run("goto", e.site.url("/console"))
	mustContain(t, out, "- Console: 2 new errors, 1 new warnings")

	out = e.run("goto", e.site.url("/spa"))
	e.run("click", ref(t, out, `button "Load"`))
	out = e.run("wait", "--text", "results LOADED") // case-insensitive
	mustContain(t, out, `Text "results LOADED" appeared`, "paragraph", "Results loaded")
	mustContain(t, e.run("wait", "--gone", "Spinner here"), "disappeared")
	mustContain(t, e.fail("wait", "--text", "never there", "--timeout", "300"), `timed out after 300ms waiting for text "never there" to appear`)
	mustContain(t, e.run("wait", "--url", "**/spa"), "URL is "+e.site.url("/spa"))
	mustContain(t, e.fail("wait", "--url", "**/elsewhere", "--timeout", "200"), "timed out after 200ms", "/spa")
	mustContain(t, e.run("wait", "--load"), "Page loaded")
	start := time.Now()
	mustContain(t, e.run("wait", "120"), "Waited 120ms")
	if time.Since(start) < 120*time.Millisecond {
		t.Error("wait returned early")
	}
}

func TestLargeSnapshotGoesToFileAndFind(t *testing.T) {
	e := shared(t)
	out := e.run("goto", e.site.url("/long"))
	mustContain(t, out, "- [Snapshot](", "too many to show", "`find <text>`", `button "Item number 0"`)
	mustNotContain(t, out, `button "Item number 399"`)
	if len(out) > 4000 {
		t.Errorf("the reply for a large page is %d chars", len(out))
	}
	file := out[strings.Index(out, "- [Snapshot](")+len("- [Snapshot]("):]
	file = file[:strings.Index(file, ")")]
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), `button "Item number 399"`) {
		t.Fatalf("snapshot file %s incomplete: %v", file, err)
	}

	found := e.run("find", "item number 250")
	mustContain(t, found, `Found 1 matching line(s) for "item number 250"`, `button "Item number 250" [ref=`, `button "Item number 247"`, `button "Item number 253"`)
	mustNotContain(t, found, `"Item number 100"`, "### Snapshot")
	target := ref(t, found, `button "Item number 250"`)
	mustContain(t, e.run("click", target), `Clicked button "Item number 250"`)
	mustContain(t, e.run("find", "--regex", `Item number 39[89]`), "Found 2 matching line(s)")
	mustContain(t, e.run("find", "--regex", `/CATALOGUE/i`), `heading "Catalogue"`)
	mustContain(t, e.run("find", "no such thing"), "No matches")
	mustContain(t, e.fail("find", "--regex", "("), "invalid regular expression")

	// A subtree or depth-limited snapshot is small enough to inline.
	e.run("goto", e.site.url("/form"))
	whole := e.run("snapshot")
	sub := e.run("snapshot", ref(t, whole, `combobox "Color"`))
	mustContain(t, sub, "```yaml\n- combobox \"Color\"", `  - option "Red"`)
	mustNotContain(t, sub, "heading", "Submit")
	shallow := e.run("snapshot", "--depth", "1")
	mustContain(t, shallow, "- form [ref=", `  - combobox "Color"`)
	mustNotContain(t, shallow, "option")
}

func TestCookies(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.run("open", e.site.url("/set-cookie?v=abc123"))
	e.run("eval", "document.cookie = 'pref=blue'")
	list := e.run("cookie-list")
	mustContain(t, list, "sid=abc123 (domain: 127.0.0.1, path: /, expires: ", "pref=blue (domain: 127.0.0.1, path: /)")
	mustContain(t, e.run("cookie-list", "--domain", "127.0.0.1"), "sid=abc123")
	mustContain(t, e.run("cookie-list", "--domain", "example.com"), "No cookies.")
	mustContain(t, e.run("goto", e.site.url("/whoami")), "sid is abc123")
	mustContain(t, e.run("cookie-clear"), "Cleared all cookies")
	mustContain(t, e.run("cookie-list"), "No cookies.")
	mustContain(t, e.run("reload"), "sid is none")
}

func TestStateSaveLoadAcrossIsolatedSession(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.run("open", e.site.url("/set-cookie?v=persisted"))
	e.run("goto", e.site.url("/storage"))
	e.run("eval", "localStorage.setItem('token', 'tok-1')")
	saved := e.run("state-save", "auth.json")
	mustContain(t, saved, "Saved 1 cookie(s) and localStorage of 1 origin(s) to auth.json")
	var state storageState
	data, err := os.ReadFile(filepath.Join(e.cwd, "auth.json"))
	if err != nil || json.Unmarshal(data, &state) != nil {
		t.Fatalf("state file: %v", err)
	}
	if len(state.Cookies) != 1 || state.Cookies[0].Name != "sid" || len(state.Origins) != 1 || state.Origins[0].Origin != e.site.url("") {
		t.Fatalf("unexpected state %+v", state)
	}

	// A second, isolated session starts empty and takes over the state.
	out := e.run("-s=other", "open", "--isolated", e.site.url("/whoami"))
	mustContain(t, out, `Browser session "other" is open (headless, profile: isolated (temporary profile))`, "sid is none")
	mustContain(t, e.run("-s=other", "state-load", "auth.json"), "Loaded 1 cookie(s) and localStorage of 1 origin(s)")
	mustContain(t, e.run("-s=other", "tab-list"), "- 0: (current)") // the helper tab is gone
	mustNotContain(t, e.run("-s=other", "tab-list"), "- 1:")
	mustContain(t, e.run("-s=other", "reload"), "sid is persisted")
	e.run("--session", "other", "goto", e.site.url("/storage"))
	mustContain(t, e.run("--session=other", "eval", "localStorage.token + '/' + localStorage.theme"), `"tok-1/dark"`)

	if st := e.m.Status(); len(st) != 2 || st[0].Name != "default" || st[1].Name != "other" {
		t.Fatalf("status = %+v", st)
	}
	mustContain(t, e.run("status"), "- default: headless, 1 tab(s)", "- other: headless")
	mustContain(t, e.run("-s=other", "close"), `Closed browser session "other"`)
	mustContain(t, e.run("-s=other", "close"), "is not open")
	mustContain(t, e.fail("-s=../x", "open"), "invalid session name")
}

func TestPersistentProfileSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, envOptions{dir: dir})
	e.run("open", e.site.url("/set-cookie?v=kept"))
	e.run("goto", e.site.url("/storage"))
	e.run("eval", "localStorage.setItem('draft', 'hello')")
	e.m.Close()
	if left := e.m.Status(); len(left) != 0 {
		t.Fatalf("sessions after Close: %v", left)
	}

	// A new Manager on the same directory (as after an Artus restart) finds
	// the logins of the default session's profile.
	e2 := newEnv(t, envOptions{dir: dir})
	out := e2.run("open", e.site.url("/whoami"))
	mustContain(t, out, "sid is kept")
	e2.run("goto", e.site.url("/storage"))
	mustContain(t, e2.run("eval", "localStorage.draft"), `"hello"`)
	// --isolated on a running session restarts it with a throwaway profile.
	out = e2.run("open", "--isolated", e.site.url("/whoami"))
	mustContain(t, out, "Restarted the browser", "isolated (temporary profile)", "sid is none")
}

func TestBrowserCrashIsRecovered(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.run("open", e.site.url("/"))
	s := e.m.lookup("default")
	s.proc.kill()
	e.waitEvent("session_close")
	if len(e.m.Status()) != 0 {
		t.Fatal("a dead session is still listed")
	}
	// The next command starts a fresh browser without being asked to.
	out := e.run("goto", e.site.url("/form"))
	mustContain(t, out, "- Page Title: Sign in")
	if got := strings.Join(e.eventTypes(), ","); got != "session_open,session_close,session_open" {
		t.Errorf("events = %s", got)
	}
	// A user closing the last window of a headed browser looks the same:
	// here the page closes its own tab, which must not break the session.
	e.run("eval", "window.close()")
	mustContain(t, e.run("goto", e.site.url("/")), "- Page Title: Home")
}

func TestStaleBrowserOnProfileIsReplaced(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.run("open", e.site.url("/set-cookie?v=before"))
	// Simulate an Artus process that died without closing its browser: the
	// browser keeps running on the profile while a new Manager starts.
	orphan := e.m.lookup("default")
	e2 := newEnv(t, envOptions{dir: e.dir})
	out := e2.run("open", e.site.url("/whoami"))
	mustContain(t, out, "sid is before")
	select {
	case <-orphan.proc.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the stale browser was not shut down")
	}
}

func TestAttachToExistingBrowser(t *testing.T) {
	exe := testChrome(t)
	profile := t.TempDir()
	proc, endpoint, err := launchBrowser(context.Background(), launchSpec{exe: exe, userDataDir: profile, headless: true, noSandbox: true, cfg: defaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer proc.kill()
	httpEndpoint := "http://" + strings.TrimPrefix(endpoint[:strings.Index(endpoint, "/devtools")], "ws://")
	e := newEnv(t, envOptions{config: func(cfg *config) { cfg.CDPEndpoint = httpEndpoint }})
	out := e.run("open", e.site.url("/"))
	mustContain(t, out, "attached via cdpEndpoint", "- Page Title: Home")
	mustContain(t, e.run("doctor"), "cdpEndpoint is set")
	e.run("close")
	// Closing the session only disconnects; the browser is not ours to stop.
	select {
	case <-proc.exited:
		t.Fatal("closing an attached session killed the external browser")
	case <-time.After(300 * time.Millisecond):
	}
	mustContain(t, e.run("open"), "- Page Title: Home") // and it kept its tab
}

func TestNetworkOriginRules(t *testing.T) {
	var e *env
	e = newEnv(t, envOptions{config: func(cfg *config) {
		cfg.Network.AllowedOrigins = []string{"http://127.0.0.1:*"}
	}})
	out := e.run("open", e.site.url("/frames"))
	// The localhost frame is another origin and is not on the allow list.
	mustContain(t, out, `iframe "same"`, "Inner button")
	if strings.Count(out, "Inner button") != 1 {
		t.Errorf("the blocked frame still loaded:\n%s", out)
	}
	mustContain(t, e.fail("goto", e.site.otherURL("/")), "is blocked by the network.allowedOrigins/blockedOrigins configuration")
	mustContain(t, e.run("eval", "fetch('"+e.site.otherURL("/")+"').then(() => 'reached', () => 'blocked')"), `"blocked"`)
	mustContain(t, e.run("eval", "fetch('/reset').then(() => 'reached', () => 'blocked')"), `"reached"`)
}

func TestConfigAndHelp(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Options{Dir: dir})
	defer m.Close()
	run := func(args ...string) string {
		t.Helper()
		res, err := m.Run(context.Background(), dir, args)
		if err != nil {
			t.Fatal(err)
		}
		return res.Text
	}
	// The file is created with defaults on first use.
	out := run("config")
	mustContain(t, out, "Configuration file: config.json", `"headless": "auto"`, `"inlineMaxChars": 6000`, `"action": 5000`)
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
	// Partial files keep defaults for what they omit.
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"headless": true, "timeouts": {"action": 1234}}`), 0o644)
	out = run("config")
	mustContain(t, out, `"headless": true`, `"action": 1234`, `"navigation": 60000`, `"width": 1280`)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"headless": tru`), 0o644)
	if _, err := m.Run(context.Background(), dir, []string{"config"}); err == nil || !strings.Contains(err.Error(), "config.json is not valid") {
		t.Fatalf("broken config: %v", err)
	}
	os.Remove(filepath.Join(dir, "config.json"))

	help := run("help")
	if help != Help() || help != run() {
		t.Error("help, no arguments and Help() should agree")
	}
	for _, spec := range commands {
		if !strings.Contains(help, "\n  "+spec.name) {
			t.Errorf("help does not list %s", spec.name)
		}
	}
	mustContain(t, help, "-s=<name>", "--timeout <ms>", "handoff <reason>", "back|go-back")
	mustContain(t, run("help", "click"), "artus browser click <ref>")
	mustContain(t, run("status"), "No browser sessions are running")
	if _, err := m.Run(context.Background(), dir, []string{"frobnicate"}); err == nil || !strings.Contains(err.Error(), `unknown command "frobnicate"`) {
		t.Fatalf("unknown command: %v", err)
	}
}

func TestLaunchConfiguration(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "init.js"), []byte("window.injectedMarker = 'from init script';"), 0o644)
	e := newEnv(t, envOptions{dir: dir, config: func(cfg *config) {
		cfg.Locale, cfg.Timezone = "de-DE", "Asia/Tokyo"
		cfg.Viewport = viewport{Width: 900, Height: 500}
		cfg.InitScripts = []string{"init.js"}
		cfg.Args = []string{"--user-agent=ArtusTest/1.0"}
		cfg.DownloadsDir = filepath.Join(dir, "dl")
		cfg.Snapshot.InlineMaxChars = 120
	}})
	out := e.run("open", e.site.url("/frames"))
	mustContain(t, out, "- [Snapshot](") // even this small page exceeds 120 characters
	got := e.run("eval", "[navigator.language, Intl.DateTimeFormat().resolvedOptions().timeZone, innerWidth, innerHeight, navigator.userAgent, window.injectedMarker].join('|')")
	mustContain(t, got, `"de-DE|Asia/Tokyo|900|500|ArtusTest/1.0|from init script"`)
	// Init scripts also run in frames, including out-of-process ones.
	cross := ref(t, e.run("find", "Inner button"), `button "Inner button"`)
	mustContain(t, e.run("eval", "el => el.ownerDocument.defaultView.injectedMarker", cross), "from init script")

	out = e.run("goto", e.site.url("/download-page"))
	e.run("click", "a")
	if ev := e.waitEvent("download"); ev.Path != filepath.Join(dir, "dl", "report.txt") {
		t.Errorf("download landed at %s", ev.Path)
	}
	// Timeouts are read from the file for every command.
	cfg := must(loadConfig(dir))
	cfg.Timeouts.Action = 150
	if err := writeConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	mustContain(t, e.fail("click", "#never"), "timed out after 150ms", "no element matches the selector")

	missing := newEnv(t, envOptions{config: func(cfg *config) { cfg.InitScripts = []string{"nope.js"} }})
	mustContain(t, missing.fail("open"), "initScripts", "nope.js")
}
