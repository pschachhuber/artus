package main

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pschachhuber/artus/internal/store"
	"github.com/pschachhuber/artus/internal/tailscale"
)

// machine is a pretend server for setup to work on: a Tailscale client that
// answers like the real one, a service manager that records what it is asked,
// and the configuration setup reads and writes.
type machine struct {
	installed bool   // Tailscale is on the machine
	state     string // Tailscale's backend state
	owner     string // login of the account the machine belongs to; "" for a tagged one
	denied    bool   // only root may change Tailscale's settings
	served    string // what Tailscale proxies to at the root of port 443

	cfg     store.Config
	saved   []store.Remote
	ran     []string // every command, as typed
	pid     int      // a running Artus; 0 for none
	managed bool     // ... and whether the service manager started it
	stopped []int
	out     strings.Builder
}

func newMachine() *machine {
	return &machine{installed: true, state: "Running", owner: "paul@example.com",
		cfg: store.Config{Server: store.Server{Host: "127.0.0.1", Port: 7717}, Remote: store.Remote{Users: []string{}}}}
}

func (m *machine) tailscale(_ context.Context, tty *tailscale.Terminal, argv ...string) ([]byte, error) {
	m.ran = append(m.ran, strings.Join(argv, " "))
	root := argv[0] == "sudo"
	if root {
		argv = argv[1:]
	}
	denied := errors.New("Access denied: prefs write access denied\n\nUse 'sudo tailscale up'.")
	switch line := strings.Join(argv[1:], " "); {
	case line == "status --json --peers=false":
		self := `{"DNSName":"artus-vm.tail1234.ts.net.","UserID":7}`
		if m.owner == "" {
			self = `{"DNSName":"artus-vm.tail1234.ts.net.","UserID":7,"Tags":["tag:server"]}`
		}
		if m.state != "Running" {
			self = `{"DNSName":"","UserID":0}`
		}
		return []byte(fmt.Sprintf(`{"BackendState":%q,"Self":%s,"User":{"7":{"LoginName":%q}},"CertDomains":["artus-vm.tail1234.ts.net"]}`, m.state, self, m.owner)), nil
	case line == "up":
		if m.denied && !root {
			return nil, denied
		}
		fmt.Fprintln(tty.Out, "To authenticate, visit: https://login.tailscale.com/a/0123456789")
		m.state = "Running"
		return nil, nil
	case line == "serve status --json":
		if m.served == "" {
			return []byte("{}"), nil
		}
		return []byte(fmt.Sprintf(`{"TCP":{"443":{"HTTPS":true}},"Web":{"artus-vm.tail1234.ts.net:443":{"Handlers":{"/":{"Proxy":%q}}}}}`, m.served)), nil
	case strings.HasPrefix(line, "serve --bg --https=443 "):
		if m.denied && !root {
			return nil, denied
		}
		m.served = argv[len(argv)-1]
		return nil, nil
	case line == "serve --https=443 off":
		if m.denied && !root {
			return nil, denied
		}
		m.served = ""
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command: %s", strings.Join(argv, " "))
}

// setup returns a setup on this machine with the given answers typed in.
// A nil answers means nobody is at the terminal.
func (m *machine) setup(t *testing.T, kind string, answers []string) *setup {
	t.Helper()
	run := func(name string, args ...string) error {
		line := strings.Join(append([]string{name}, args...), " ")
		m.ran = append(m.ran, line)
		switch {
		case name == "sh": // Tailscale's installer
			m.installed = true
		case strings.Contains(line, "systemctl restart"), strings.Contains(line, "launchctl bootstrap"):
			m.pid, m.managed = 4242, true
		case strings.Contains(line, "systemctl disable"), strings.Contains(line, "launchctl bootout"):
			m.pid = 0
		}
		return nil
	}
	svc := &service{Kind: kind, Path: filepath.Join(t.TempDir(), "artus.service"), Exe: "/home/paul/.local/bin/artus", User: "paul", Home: "/home/paul",
		Env: map[string]string{"PATH": "/home/paul/.local/bin:/usr/bin"}, run: run}
	return &setup{
		out: &m.out, in: bufio.NewReader(strings.NewReader(strings.Join(answers, "\n") + "\n")),
		tty:         &tailscale.Terminal{In: strings.NewReader(""), Out: &m.out, Err: &m.out},
		interactive: answers != nil, goos: "linux",
		tailscale: func() (*tailscale.CLI, bool) {
			return &tailscale.CLI{Path: "tailscale", Run: m.tailscale}, m.installed
		},
		run: run, service: svc,
		config: func() (store.Config, error) { return m.cfg, nil },
		saveRemote: func(r store.Remote) error {
			m.saved = append(m.saved, r)
			m.cfg.Remote = r
			return nil
		},
		running: func() (int, bool, bool) { return m.pid, m.managed, m.pid != 0 },
		stop: func(pid int) error {
			m.stopped = append(m.stopped, pid)
			m.pid = 0
			return nil
		},
	}
}

func (m *machine) did(part string) bool {
	for _, line := range m.ran {
		if strings.Contains(line, part) {
			return true
		}
	}
	return false
}

func TestSetupOnAConnectedMachine(t *testing.T) {
	m := newMachine()
	s := m.setup(t, "systemd", []string{"", "y"}) // Enter and "y": yes to the service, yes to nothing else asked
	if err := s.do(nil); err != nil {
		t.Fatalf("%v\n%s", err, m.out.String())
	}
	want := store.Remote{URL: "https://artus-vm.tail1234.ts.net", Users: []string{"paul@example.com"}}
	if len(m.saved) != 1 || m.saved[0].URL != want.URL || strings.Join(m.saved[0].Users, ",") != "paul@example.com" {
		t.Errorf("saved %+v, want %+v", m.saved, want)
	}
	if m.served != "http://127.0.0.1:7717" {
		t.Errorf("Tailscale serves %q", m.served)
	}
	for _, cmd := range []string{"sudo install -m 0644", "sudo systemctl daemon-reload", "sudo systemctl enable artus.service", "sudo systemctl restart artus.service"} {
		if !m.did(cmd) {
			t.Errorf("never ran %q:\n%s", cmd, strings.Join(m.ran, "\n"))
		}
	}
	out := m.out.String()
	for _, part := range []string{"https://artus-vm.tail1234.ts.net", "paul@example.com", "home screen", "artus url"} {
		if !strings.Contains(out, part) {
			t.Errorf("the summary does not mention %q:\n%s", part, out)
		}
	}

	// Running it again changes nothing on the tailnet and keeps the account.
	before := len(m.ran)
	if err := m.setup(t, "systemd", []string{"n"}).do(nil); err != nil {
		t.Fatal(err)
	}
	for _, line := range m.ran[before:] {
		if strings.Contains(line, "serve --bg") || strings.Contains(line, "systemctl") {
			t.Errorf("second run did %q", line)
		}
	}
	if got := m.cfg.Remote.Users; len(got) != 1 {
		t.Errorf("accounts after a second run: %v", got)
	}

	// --user lets a second account in, next to the owner.
	if err := m.setup(t, "systemd", []string{"n"}).do([]string{"anna@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.cfg.Remote.Users, ","); got != "paul@example.com,anna@example.com" {
		t.Errorf("accounts = %s", got)
	}
}

func TestSetupWithNobodyAtTheTerminal(t *testing.T) {
	// Nothing that needs a yes happens: no installer, no sign-in, no service.
	m := newMachine()
	m.installed = false
	if err := m.setup(t, "systemd", nil).do(nil); err != nil {
		t.Fatal(err)
	}
	if len(m.ran) != 0 || len(m.saved) != 0 {
		t.Errorf("did things unasked: %v %v", m.ran, m.saved)
	}
	if out := m.out.String(); !strings.Contains(out, "curl -fsSL https://tailscale.com/install.sh | sh") || !strings.Contains(out, "not reachable from other devices yet") {
		t.Errorf("does not say what to do by hand:\n%s", out)
	}

	m = newMachine()
	m.state = "NeedsLogin"
	if err := m.setup(t, "systemd", nil).do(nil); err != nil {
		t.Fatal(err)
	}
	if m.did(" up") || m.did("systemctl") || !strings.Contains(m.out.String(), "sudo tailscale up") {
		t.Errorf("logged out, unattended: %v\n%s", m.ran, m.out.String())
	}

	// A connected machine needs no answer to be published; the service still does.
	m = newMachine()
	if err := m.setup(t, "systemd", nil).do(nil); err != nil {
		t.Fatal(err)
	}
	if len(m.saved) != 1 || m.did("systemctl") {
		t.Errorf("connected, unattended: saved %v, ran %v", m.saved, m.ran)
	}
}

func TestSetupFromNothing(t *testing.T) {
	// --yes on a fresh server: Tailscale is installed, signed in to through sudo, and serves Artus.
	m := newMachine()
	m.installed, m.state, m.denied = false, "NeedsLogin", true
	s := m.setup(t, "systemd", nil)
	s.yes = true
	if err := s.do(nil); err != nil {
		t.Fatalf("%v\n%s", err, m.out.String())
	}
	for _, cmd := range []string{"sh -c f=$(mktemp) && curl -fsSL https://tailscale.com/install.sh", "sudo tailscale up", "sudo tailscale serve --bg --https=443 http://127.0.0.1:7717", "systemctl restart"} {
		if !m.did(cmd) {
			t.Errorf("never ran %q:\n%s", cmd, strings.Join(m.ran, "\n"))
		}
	}
	if !strings.Contains(m.out.String(), "https://login.tailscale.com/a/") {
		t.Errorf("Tailscale's sign-in link was not shown:\n%s", m.out.String())
	}
	if len(m.saved) != 1 || m.saved[0].URL == "" {
		t.Errorf("saved %v", m.saved)
	}

	// As root nothing goes through sudo.
	m = newMachine()
	s = m.setup(t, "systemd", nil)
	s.yes, s.root, s.service.Root, s.service.User = true, true, true, "root"
	if err := s.do(nil); err != nil {
		t.Fatal(err)
	}
	if m.did("sudo") || !m.did("systemctl restart artus.service") || !strings.Contains(m.out.String(), "you are root") {
		t.Errorf("as root: %v\n%s", m.ran, m.out.String())
	}
}

func TestSetupOnATaggedMachine(t *testing.T) {
	m := newMachine()
	m.owner = ""
	// Asked who may come in, and told.
	if err := m.setup(t, "", []string{"paul@example.com"}).do(nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.cfg.Remote.Users, ","); got != "paul@example.com" {
		t.Errorf("accounts = %q\n%s", got, m.out.String())
	}
	// Unattended, the flag says it; without it nobody is let in and setup says so.
	m = newMachine()
	m.owner = ""
	if err := m.setup(t, "", nil).do([]string{"anna@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.cfg.Remote.Users, ","); got != "anna@example.com" {
		t.Errorf("accounts = %q", got)
	}
	m = newMachine()
	m.owner = ""
	if err := m.setup(t, "", nil).do(nil); err != nil {
		t.Fatal(err)
	}
	if len(m.cfg.Remote.Users) != 0 || !strings.Contains(m.out.String(), "artus setup --user") {
		t.Errorf("no account: %v\n%s", m.cfg.Remote.Users, m.out.String())
	}
}

func TestSetupTakesOverAHandStartedArtus(t *testing.T) {
	m := newMachine()
	m.pid = 1001
	s := m.setup(t, "launchd", nil)
	s.yes = true
	if err := s.do(nil); err != nil {
		t.Fatalf("%v\n%s", err, m.out.String())
	}
	if len(m.stopped) != 1 || m.stopped[0] != 1001 || !m.managed {
		t.Errorf("stopped %v, managed %v", m.stopped, m.managed)
	}
	if !m.did("launchctl bootstrap gui/") || m.did("sudo launchctl") {
		t.Errorf("launchd: %v", m.ran)
	}
	if b, err := os.ReadFile(s.service.Path); err != nil || !strings.Contains(string(b), launchdLabel) {
		t.Errorf("agent file: %v %s", err, b)
	}
	// One that the service manager started is left running and simply restarted.
	m = newMachine()
	m.pid, m.managed = 2002, true
	s = m.setup(t, "systemd", nil)
	s.yes = true
	if err := s.do(nil); err != nil {
		t.Fatal(err)
	}
	if len(m.stopped) != 0 {
		t.Errorf("stopped the service's own Artus: %v", m.stopped)
	}
}

func TestSetupRemove(t *testing.T) {
	m := newMachine()
	s := m.setup(t, "systemd", nil)
	s.yes = true
	if err := s.do(nil); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.service.Path, []byte(s.service.definition()), 0o644) // what `sudo install` would have written
	m.denied = true
	if err := s.undo(); err != nil {
		t.Fatalf("%v\n%s", err, m.out.String())
	}
	if m.served != "" || m.cfg.Remote.URL != "" || len(m.cfg.Remote.Users) != 0 {
		t.Errorf("still set up: serves %q, remote %+v", m.served, m.cfg.Remote)
	}
	for _, cmd := range []string{"sudo tailscale serve --https=443 off", "sudo systemctl disable --now artus.service", "sudo rm -f " + s.service.Path} {
		if !m.did(cmd) {
			t.Errorf("never ran %q:\n%s", cmd, strings.Join(m.ran, "\n"))
		}
	}
	// Nothing set up: nothing to remove, and no error.
	m = newMachine()
	if err := m.setup(t, "systemd", nil).undo(); err != nil || len(m.ran) != 0 {
		t.Errorf("remove on a machine that was never set up: %v %v", err, m.ran)
	}
}

func TestAsk(t *testing.T) {
	for answer, want := range map[string]bool{"\n": true, "y\n": true, "Yes\n": true, "n\n": false, "no\n": false, "": false, "what\n": false} {
		s := &setup{out: io.Discard, in: bufio.NewReader(strings.NewReader(answer)), interactive: true}
		if got := s.ask("Go on?"); got != want {
			t.Errorf("answer %q: %v", answer, got)
		}
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer null.Close()
	if terminal(null) {
		t.Error("the null device was taken for a terminal")
	}
	var users userList
	users.Set("a@x.com,b@x.com")
	users.Set("c@x.com")
	if strings.Join(users, " ") != "a@x.com b@x.com c@x.com" {
		t.Errorf("users = %v", users)
	}
}

func TestServiceDefinitions(t *testing.T) {
	s := &service{Kind: "systemd", Exe: `/home/pa ul/.local/bin/artus`, User: "paul", Home: "/home/pa ul",
		Env: map[string]string{"PATH": `/home/pa ul/bin:/usr/bin`, "ARTUS_HOME": `/data/100%/a"b\c$d`}}
	unit := s.definition()
	for _, line := range []string{
		"User=paul",
		"WorkingDirectory=/home/pa ul",
		`Environment="ARTUS_HOME=/data/100%%/a\"b\\c$d"`,
		`Environment="PATH=/home/pa ul/bin:/usr/bin"`,
		`Environment="ARTUS_SERVICE=1"`,
		`ExecStart="/home/pa ul/.local/bin/artus" serve --no-open`,
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, "\n"+line+"\n") {
			t.Errorf("unit lacks the line %s:\n%s", line, unit)
		}
	}
	if strings.Index(unit, "ARTUS_HOME") > strings.Index(unit, "PATH=") {
		t.Error("environment not in a fixed order")
	}

	s = &service{Kind: "launchd", Exe: "/Users/p&l/bin/artus", User: "paul", Home: "/Users/p&l", Env: map[string]string{"PATH": "/usr/bin:/a<b"}}
	var doc struct {
		Dict struct {
			Keys    []string `xml:"key"`
			Strings []string `xml:"string"`
			Array   struct {
				Strings []string `xml:"string"`
			} `xml:"array"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(s.definition()), &doc); err != nil {
		t.Fatalf("the agent file is not XML: %v\n%s", err, s.definition())
	}
	if got := strings.Join(doc.Dict.Array.Strings, " "); got != "/Users/p&l/bin/artus serve --no-open" {
		t.Errorf("program arguments = %q", got)
	}
	if !strings.Contains(s.definition(), "<key>ARTUS_SERVICE</key>") {
		t.Error("the agent does not mark the Artus it starts")
	}
	if doc.Dict.Strings[0] != launchdLabel || !strings.Contains(strings.Join(doc.Dict.Keys, " "), "RunAtLoad") {
		t.Errorf("agent file: %+v", doc.Dict)
	}
}
