package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pschachhuber/artus/defaults"
	"github.com/pschachhuber/artus/internal/agent"
	"github.com/pschachhuber/artus/internal/browser"
	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/sched"
	"github.com/pschachhuber/artus/internal/secrets"
	"github.com/pschachhuber/artus/internal/server"
	"github.com/pschachhuber/artus/internal/store"
	"github.com/pschachhuber/artus/internal/update"
	"github.com/pschachhuber/artus/web"
)

// app is one running Artus: everything wired together.
type app struct {
	home    *store.Home
	secrets *secrets.Store
	engine  *agent.Engine
	sched   *sched.Scheduler
	browser *browser.Manager
	server  *server.Server
	updates *update.Service
	http    *http.Server
	cancel  context.CancelFunc
}

const stateFile = "server.json" // where a running Artus records its address

func newApp(dir string) (*app, error) {
	home, err := store.Open(dir, defaults.Home())
	if err != nil {
		return nil, err
	}
	a := &app{home: home}
	a.secrets, err = secrets.Open(home.Path(store.SecretsFile), home.Path("pass-session"), func(b []byte) error {
		return home.WriteLocked(store.SecretsFile, b)
	})
	if err != nil {
		return nil, err
	}

	key := func(name string) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			v, err := a.secrets.Get(ctx, name, "Artus: model request")
			if err != nil {
				return "", fmt.Errorf("no %s. Add it in Settings", name)
			}
			return v, nil
		}
	}
	reg := llm.NewRegistry()
	reg.Register("anthropic", llm.NewAnthropic(key("ANTHROPIC_API_KEY")))
	reg.Register("openai", llm.NewOpenAI(key("OPENAI_API_KEY")))
	reg.Register("openrouter", llm.NewOpenRouter(key("OPENROUTER_API_KEY")))
	if os.Getenv("ARTUS_MOCK") != "" {
		reg.Register("mock", &llm.Mock{Delay: 25 * time.Millisecond})
	}

	prompt := defaults.Prompt
	if path := os.Getenv("ARTUS_PROMPT"); path != "" { // replace the base prompt, for experiments
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("ARTUS_PROMPT: %w", err)
		}
		prompt = string(b)
	}
	a.engine = agent.New(home, a.secrets, reg, prompt)
	a.engine.BinDir = launcher(home)
	a.sched, err = sched.New(home.Path(store.ScheduleFile), func(b []byte) error {
		return home.WriteLocked(store.ScheduleFile, b)
	}, a.engine.Fire)
	if err != nil {
		return nil, err
	}
	a.engine.Sched = a.sched
	a.sched.OnError = func(err error) { a.server.Publish("alert", "The schedule could not be saved: "+err.Error()) }
	// The monthly maintenance trigger is set up once; if the user cancels it, it stays cancelled.
	if marker := home.Path("tasks", "maintenance", ".scheduled"); !exists(marker) {
		if _, err := a.sched.Add("task:maintenance", "cron 0 4 1 * *", "Monthly maintenance"); err == nil {
			os.WriteFile(marker, nil, 0o600)
		}
	}

	a.browser = browser.NewManager(browser.Options{
		Dir: home.Path("browser"),
		ResolveSecret: func(name string) (string, error) {
			return a.secrets.Use(context.Background(), name, "Artus: browser form")
		},
		TOTP: func(name string) (string, error) {
			return a.secrets.TOTP(context.Background(), name, "Artus: browser sign-in code")
		},
		Redact:  a.secrets.Redact,
		OnEvent: func(ev browser.Event) { a.server.Publish("browser", ev) },
	})
	a.server = server.New(server.Server{
		Home: home, Engine: a.engine, Secrets: a.secrets, LLM: reg, Sched: a.sched,
		Browser: browserAdapter{a.browser}, UI: web.Files(), Version: version,
	})
	a.updates = &update.Service{
		Updater: update.New(version),
		Mode:    func() string { return a.home.Config().Update },
		Idle:    a.idle,
		Changed: func(st update.Status) { a.server.Publish("update", st) },
		Restart: a.restart,
	}
	return a, nil
}

// idle reports whether restarting would interrupt nothing: no chat or task is
// running and no one is being waited for in the browser.
func (a *app) idle() bool {
	if len(a.engine.Running()) > 0 {
		return false
	}
	for _, s := range a.browser.Status() {
		if s.Handoff != nil {
			return false
		}
	}
	return true
}

// restart stops everything and runs the freshly installed program in this
// process's place, with the same arguments.
func (a *app) restart() {
	fmt.Println("Restarting into the new version…")
	exe := a.updates.Updater.Exe
	a.close()
	env := append(os.Environ(), "ARTUS_UPDATED_FROM="+version)
	if err := update.Restart(exe, env); err != nil {
		fmt.Fprintln(os.Stderr, "artus: could not restart:", err, "(start Artus again to use the new version)")
		os.Exit(1)
	}
}

// launcher makes the command `artus` available to everything the agent runs,
// whatever this executable is called and wherever it lives (a release
// download is named artus_windows_amd64.exe, for one). It writes tiny
// wrapper scripts into the home and returns their folder for the PATH.
func launcher(home *store.Home) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := home.Path("bin")
	if os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	posix := "#!/bin/sh\nexec '" + strings.ReplaceAll(filepath.ToSlash(exe), "'", `'\''`) + "' \"$@\"\n"
	os.WriteFile(filepath.Join(dir, "artus"), []byte(posix), 0o700)
	if runtime.GOOS == "windows" {
		os.WriteFile(filepath.Join(dir, "artus.cmd"), []byte("@\""+exe+"\" %*\r\n"), 0o700)
	}
	return dir
}

// listen starts serving and returns the sign-in link.
func (a *app) listen(port int) (string, error) {
	cfg := a.home.Config().Server
	if port == 0 {
		port = cfg.Port
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, fmt.Sprint(port)))
	if err != nil {
		return "", fmt.Errorf("cannot listen on %s:%d: %w (try --port)", cfg.Host, port, err)
	}
	base := fmt.Sprintf("http://%s", net.JoinHostPort(displayHost(cfg.Host), fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)))
	a.engine.ServerURL = base
	a.http = &http.Server{Handler: a.server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go a.http.Serve(ln)

	url := base + "/?token=" + cfg.Token
	state, _ := json.Marshal(map[string]any{"url": base, "pid": os.Getpid()})
	os.WriteFile(a.home.Path(stateFile), state, 0o600)
	return url, nil
}

func (a *app) close() {
	os.Remove(a.home.Path(stateFile))
	if a.cancel != nil {
		a.cancel()
	}
	a.engine.Shutdown()
	a.browser.Close()
	if a.http != nil {
		shutdown(a.http)
	}
}

// browserAdapter presents the browser driver through the server's interface.
type browserAdapter struct{ m *browser.Manager }

func (b browserAdapter) Run(ctx context.Context, cwd string, args []string) (string, []string, error) {
	res, err := b.m.Run(ctx, cwd, args)
	return res.Text, res.Files, err
}
func (b browserAdapter) Status() any                   { return b.m.Status() }
func (b browserAdapter) ResolveHandoff(id string) bool { return b.m.ResolveHandoff(id) }
func (b browserAdapter) Screencast(ctx context.Context, session string, onFrame func([]byte)) error {
	return b.m.Screencast(ctx, session, onFrame)
}
func (b browserAdapter) Input(session string, raw json.RawMessage) error {
	var ev browser.InputEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return err
	}
	return b.m.Input(session, ev)
}

// client calls the HTTP API of a running Artus.
type client struct{ base, token string }

func (c client) call(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach Artus at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// liveServer reports the sign-in link of an Artus already serving this home.
// The address in server.json must be on this machine and the server there
// must prove it holds this home's token, which it is never sent.
func liveServer(dir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return "", false
	}
	var state struct{ URL string }
	if json.Unmarshal(b, &state) != nil {
		return "", false
	}
	u, err := url.Parse(state.URL)
	if err != nil || u.Scheme != "http" || u.Path != "" || !loopback(u.Hostname()) {
		return "", false
	}
	token := savedToken(dir)
	nonce := store.RandomID(16)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(state.URL + "/api/hello?nonce=" + nonce)
	if err != nil {
		return "", false // stale file from a crash
	}
	defer resp.Body.Close()
	var hello struct{ Proof string }
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&hello) != nil || token == "" ||
		!hmac.Equal([]byte(hello.Proof), []byte(server.Proof(token, nonce))) {
		return "", false // some other program now has that port
	}
	return state.URL + "/?token=" + token, true
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// savedToken reads the access token from config.json without opening the
// home, which would start its own bookkeeping next to a running Artus.
func savedToken(dir string) string {
	var cfg store.Config
	b, _ := os.ReadFile(filepath.Join(dir, store.ConfigFile))
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return cfg.Server.Token
}

// connect returns a client for the running Artus. Commands started by the
// agent find it through their environment. With start set, an Artus is
// started in this process when none is running.
func connect(start bool) (client, func(), error) {
	if url, token := os.Getenv("ARTUS_URL"), os.Getenv("ARTUS_AGENT_TOKEN"); url != "" && token != "" {
		return client{url, token}, func() {}, nil
	}
	dir := store.DefaultDir()
	if url, ok := liveServer(dir); ok {
		base, token, _ := strings.Cut(url, "/?token=")
		return client{base, token}, func() {}, nil
	}
	if !start {
		return client{}, nil, errors.New("Artus is not running. Start it with: artus")
	}
	a, err := newApp(dir)
	if err != nil {
		return client{}, nil, err
	}
	cfg := a.home.Config().Server
	url, err := a.listen(cfg.Port)
	if err != nil {
		if url, err = a.listenAny(); err != nil {
			return client{}, nil, err
		}
	}
	base, token, _ := strings.Cut(url, "/?token=")
	return client{base, token}, a.close, nil
}

// listenAny serves on any free port, for one-shot runs when the usual port is taken.
func (a *app) listenAny() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return a.listen(port)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
