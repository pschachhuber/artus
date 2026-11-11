package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// testChrome locates a browser for the integration tests: ARTUS_TEST_CHROME,
// the CI image's Chrome for Testing, or whatever the resolver finds.
func testChrome(t *testing.T) string {
	t.Helper()
	candidates := []string{os.Getenv("ARTUS_TEST_CHROME"), "/opt/pw-browsers/chromium"}
	for _, kind := range browserKinds {
		if p, ok := findInstalled(kind); ok {
			candidates = append(candidates, p)
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	t.Skip("no Chromium-family browser found; set ARTUS_TEST_CHROME")
	return ""
}

// env is a Manager with its own state directory plus the fixture servers.
type env struct {
	t      *testing.T
	m      *Manager
	dir    string
	cwd    string
	site   *site
	mu     sync.Mutex
	events []Event
	texts  []string // every Result.Text and error returned so far
}

type envOptions struct {
	dir    string
	cwd    string
	redact func(string) string
	config func(cfg *config)
}

// testSecrets and testTOTP back the --secret and --totp flags in tests.
var (
	testSecrets = map[string]string{"SITE_PASSWORD": "hunter2-correct-horse"}
	testTOTP    = map[string]string{"SITE_TOTP": "492817"}
)

// newEnv starts a Manager with its own state directory and fixture site.
func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	e := &env{t: t, dir: o.dir, cwd: o.cwd, site: newSite()}
	if e.dir == "" {
		e.dir = t.TempDir()
	}
	if e.cwd == "" {
		e.cwd = t.TempDir()
	}
	e.start(o)
	t.Cleanup(e.close)
	return e
}

func (e *env) start(o envOptions) {
	cfg := defaultConfig()
	cfg.ExecutablePath, cfg.Headless, cfg.NoSandbox = testChrome(e.t), headlessOn, true
	if o.config != nil {
		o.config(&cfg)
	}
	if err := writeConfig(e.dir, cfg); err != nil {
		e.t.Fatal(err)
	}
	lookup := func(values map[string]string) func(string) (string, error) {
		return func(name string) (string, error) {
			if v, ok := values[name]; ok {
				return v, nil
			}
			return "", fmt.Errorf("not found")
		}
	}
	e.m = NewManager(Options{
		Dir:           e.dir,
		ResolveSecret: lookup(testSecrets),
		TOTP:          lookup(testTOTP),
		Redact:        o.redact,
		OnEvent: func(ev Event) {
			e.mu.Lock()
			e.events = append(e.events, ev)
			e.mu.Unlock()
		},
	})
}

func (e *env) close() {
	e.m.Close()
	e.site.close()
}

// The shared env serves the tests that only need a page to work on: they
// reuse one browser instead of launching one each.
var (
	sharedOnce sync.Once
	sharedE    *env
)

func shared(t *testing.T) *env {
	t.Helper()
	testChrome(t)
	sharedOnce.Do(func() {
		root := filepath.Join(os.TempDir(), "shared")
		sharedE = &env{t: t, dir: filepath.Join(root, "state"), cwd: filepath.Join(root, "cwd"), site: newSite()}
		os.MkdirAll(sharedE.cwd, 0o755)
		sharedE.start(envOptions{})
	})
	sharedE.t = t
	sharedE.mu.Lock()
	sharedE.events, sharedE.texts = nil, nil
	sharedE.mu.Unlock()
	return sharedE
}

// TestMain gives the run a private temporary directory, so that at the end
// every browser profile created by it can be recognised: no browser process
// may outlive the tests.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "artus-browser-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		os.Setenv(name, root)
	}
	code := m.Run()
	if sharedE != nil {
		sharedE.close()
	}
	if left := leftoverBrowsers(root); len(left) > 0 && code == 0 {
		fmt.Fprintf(os.Stderr, "browser processes left running after the tests:\n%s\n", strings.Join(left, "\n"))
		code = 1
	}
	os.RemoveAll(root)
	os.Exit(code)
}

// leftoverBrowsers lists processes still running on a profile below root
// (Linux only; elsewhere it finds nothing).
func leftoverBrowsers(root string) []string {
	var left []string
	entries, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, entry := range entries {
		data, err := os.ReadFile(entry)
		if err == nil && strings.Contains(string(data), "--user-data-dir="+root) {
			left = append(left, strings.ReplaceAll(string(data), "\x00", " "))
		}
	}
	return left
}

// try runs a command and returns its text and error.
func (e *env) try(args ...string) (string, error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := e.m.Run(ctx, e.cwd, args)
	e.mu.Lock()
	e.texts = append(e.texts, res.Text)
	if err != nil {
		e.texts = append(e.texts, err.Error())
	}
	e.mu.Unlock()
	return res.Text, err
}

// run runs a command that must succeed and returns its text.
func (e *env) run(args ...string) string {
	e.t.Helper()
	text, err := e.try(args...)
	if err != nil {
		e.t.Fatalf("artus browser %s: %v", strings.Join(args, " "), err)
	}
	return text
}

// fail runs a command that must fail and returns its error text.
func (e *env) fail(args ...string) string {
	e.t.Helper()
	text, err := e.try(args...)
	if err == nil {
		e.t.Fatalf("artus browser %s: expected an error, got:\n%s", strings.Join(args, " "), text)
	}
	return err.Error()
}

func (e *env) eventTypes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var types []string
	for _, ev := range e.events {
		types = append(types, ev.Type)
	}
	return types
}

// waitEvent blocks until an event of the given type has been emitted.
func (e *env) waitEvent(kind string) Event {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		for _, ev := range e.events {
			if ev.Type == kind {
				e.mu.Unlock()
				return ev
			}
		}
		e.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("no %q event; got %v", kind, e.eventTypes())
	return Event{}
}

// ref finds the ref of the first snapshot line containing all of the given
// fragments, e.g. ref(text, `button "Save"`).
func ref(t *testing.T, text string, fragments ...string) string {
	t.Helper()
	pattern := regexp.MustCompile(`\[ref=((?:f\d+)?e\d+)\]`)
	for _, line := range strings.Split(text, "\n") {
		ok := true
		for _, f := range fragments {
			ok = ok && strings.Contains(line, f)
		}
		if m := pattern.FindStringSubmatch(line); ok && m != nil {
			return m[1]
		}
	}
	t.Fatalf("no snapshot line with a ref contains %q in:\n%s", fragments, text)
	return ""
}

func mustContain(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func mustNotContain(t *testing.T, text string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(text, u) {
			t.Fatalf("unexpected %q in:\n%s", u, text)
		}
	}
}

// site is the fixture web site: static pages by path, plus endpoints that
// record what the browser sent.
type site struct {
	srv   *httptest.Server
	other *httptest.Server // the same handler on another origin (localhost vs 127.0.0.1)

	mu        sync.Mutex
	submitted []url.Values
	uploads   map[string]string
}

func newSite() *site {
	s := &site{uploads: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/submit", s.submit)
	mux.HandleFunc("/upload", s.upload)
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="report.txt"`)
		fmt.Fprint(w, "downloaded content")
	})
	mux.HandleFunc("/set-cookie", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: r.URL.Query().Get("v"), Path: "/", MaxAge: 3600})
		fmt.Fprint(w, "<title>Cookie set</title><p>cookie set</p>")
	})
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		value := "none"
		if c, err := r.Cookie("sid"); err == nil {
			value = c.Value
		}
		fmt.Fprintf(w, "<title>Who</title><p>sid is %s</p>", value)
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/nocontent", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		// The document arrives late, and its stylesheet later still.
		time.Sleep(400 * time.Millisecond)
		fmt.Fprint(w, `<!doctype html><title>Slow</title><link rel="stylesheet" href="/slow.css"><h1>Finally</h1>`)
	})
	mux.HandleFunc("/slow.css", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "text/css")
		fmt.Fprint(w, "h1 { color: green }")
	})
	s.srv = httptest.NewServer(mux)
	s.other = httptest.NewServer(mux)
	return s
}

func (s *site) close() {
	s.srv.Close()
	s.other.Close()
}

// url returns the address of a fixture page on the main origin.
func (s *site) url(path string) string { return s.srv.URL + path }

// otherURL returns the address of a fixture page on a second origin.
func (s *site) otherURL(path string) string {
	return strings.Replace(s.other.URL, "127.0.0.1", "localhost", 1) + path
}

func (s *site) page(w http.ResponseWriter, r *http.Request) {
	body, ok := pages[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	body = strings.ReplaceAll(body, "{{OTHER}}", s.otherURL(""))
	fmt.Fprint(w, "<!doctype html>"+body)
}

func (s *site) submit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	s.mu.Lock()
	s.submitted = append(s.submitted, r.Form)
	s.mu.Unlock()
	data, _ := json.Marshal(r.Form)
	fmt.Fprintf(w, "<!doctype html><title>Submitted</title><h1>Thanks</h1><pre>%s</pre>", data)
}

func (s *site) upload(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(1 << 20)
	var names []string
	s.mu.Lock()
	for _, headers := range r.MultipartForm.File {
		for _, h := range headers {
			f, _ := h.Open()
			buf := make([]byte, h.Size)
			f.Read(buf)
			f.Close()
			s.uploads[h.Filename] = string(buf)
			names = append(names, h.Filename)
		}
	}
	s.mu.Unlock()
	fmt.Fprintf(w, "<!doctype html><title>Uploaded</title><p>received %s</p>", strings.Join(names, ","))
}

// lastSubmit returns the fields of the most recent form submission.
func (s *site) lastSubmit(t *testing.T) url.Values {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.submitted) == 0 {
		t.Fatal("no form was submitted")
	}
	return s.submitted[len(s.submitted)-1]
}

// writeFile creates a file in the env's working directory.
func (e *env) writeFile(name, content string) string {
	e.t.Helper()
	path := filepath.Join(e.cwd, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return path
}
