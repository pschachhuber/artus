// Package tailscale drives the Tailscale program installed next to Artus.
// Nothing of Tailscale is linked in: Artus asks the command line client for
// the state of this machine and has `tailscale serve` publish its interface
// on the user's tailnet, with HTTPS, under the machine's Tailscale name.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// CLI is the Tailscale command line client.
type CLI struct {
	Path string
	// Sudo runs the client as root, for a user Tailscale does not let change
	// its settings. sudo may ask for a password on the terminal.
	Sudo bool
	// Run executes a command line and returns what it printed and, when it
	// failed, what it complained about. Tests replace it.
	Run func(ctx context.Context, tty *Terminal, argv ...string) ([]byte, error)
}

// Terminal is where a command that may talk to the user (to have them sign
// in, or turn on HTTPS) shows its messages while it runs.
type Terminal struct {
	In       io.Reader
	Out, Err io.Writer
}

// ErrDenied means Tailscale did not let this user change its settings: only
// root and the operator named with `tailscale set --operator` may.
var ErrDenied = errors.New("Tailscale lets only root change this")

// Find returns the installed client. The service manager starts Artus with a
// short PATH, so the usual places are looked at as well.
func Find() (*CLI, bool) {
	path, err := exec.LookPath("tailscale")
	if err != nil {
		path = ""
		for _, p := range places() {
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				path = p
				break
			}
		}
	}
	if path == "" {
		return nil, false
	}
	return &CLI{Path: path}, true
}

func places() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{filepath.Join(os.Getenv("ProgramFiles"), "Tailscale", "tailscale.exe")}
	case "darwin":
		return []string{"/usr/local/bin/tailscale", "/opt/homebrew/bin/tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale"}
	}
	return []string{"/usr/bin/tailscale", "/usr/local/bin/tailscale", "/usr/sbin/tailscale", "/snap/bin/tailscale"}
}

// AsRoot returns the client run through sudo.
func (c *CLI) AsRoot() *CLI {
	root := *c
	root.Sudo = true
	return &root
}

// exec runs the client with args.
func (c *CLI) exec(ctx context.Context, tty *Terminal, args ...string) ([]byte, error) {
	argv := append([]string{c.Path}, args...)
	if c.Sudo {
		argv = append([]string{"sudo"}, argv...)
	}
	run := c.Run
	if run == nil {
		run = runCommand
	}
	out, err := run(ctx, tty, argv...)
	switch {
	case err == nil:
		return out, nil
	case strings.Contains(err.Error(), "Access denied"):
		return out, ErrDenied
	}
	return out, fmt.Errorf("tailscale %s: %s", args[0], firstLine(err.Error()))
}

// runCommand runs a program. Its error is what the program wrote to explain
// itself.
func runCommand(ctx context.Context, tty *Terminal, argv ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, problem bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &problem
	if tty != nil {
		cmd.Stdin = tty.In
		cmd.Stdout = io.MultiWriter(&out, tty.Out)
		cmd.Stderr = io.MultiWriter(&problem, tty.Err)
	}
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(problem.String())
		if text == "" {
			text = strings.TrimSpace(out.String())
		}
		if text == "" {
			text = err.Error()
		}
		return out.Bytes(), errors.New(text)
	}
	return out.Bytes(), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// Status is the state of this machine on the tailnet.
type Status struct {
	Version string `json:"version"`
	State   string `json:"state"`  // Running, NeedsLogin, Stopped, Starting, NoState
	Name    string `json:"name"`   // "vm.tail1234.ts.net"; empty until connected
	User    string `json:"user"`   // login of the account the machine belongs to; empty for a tagged machine
	Tagged  bool   `json:"tagged"` // the machine belongs to a tag, not to a person
	HTTPS   bool   `json:"https"`  // the tailnet issues certificates, which serving needs
}

// Connected reports whether the machine is on a tailnet under a name.
func (s Status) Connected() bool { return s.State == "Running" && s.Name != "" }

// Status asks the client for this machine's state.
func (c *CLI) Status(ctx context.Context) (Status, error) {
	out, err := c.exec(ctx, nil, "status", "--json", "--peers=false")
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		return Status{}, err
	}
	var raw struct {
		Version      string
		BackendState string
		CertDomains  []string
		Self         *struct {
			DNSName string
			UserID  json.Number
			Tags    []string
		}
		User map[string]struct{ LoginName string }
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return Status{}, fmt.Errorf("tailscale status: unexpected output: %w", err)
	}
	st := Status{Version: firstLine(raw.Version), State: raw.BackendState, HTTPS: len(raw.CertDomains) > 0}
	if raw.Self != nil {
		st.Name = strings.ToLower(strings.TrimSuffix(raw.Self.DNSName, "."))
		st.Tagged = len(raw.Self.Tags) > 0
		if !st.Tagged {
			st.User = raw.User[raw.Self.UserID.String()].LoginName
		}
	}
	return st, nil
}

// Up connects this machine to a tailnet. Tailscale prints a link to sign in
// with on tty and waits until that is done.
func (c *CLI) Up(ctx context.Context, tty *Terminal) error {
	_, err := c.exec(ctx, tty, "up")
	return err
}

// serveConfig is the part of Tailscale's serve configuration Artus reads.
type serveConfig struct {
	TCP map[string]json.RawMessage
	Web map[string]struct {
		Handlers map[string]struct{ Proxy string }
	}
	AllowFunnel map[string]bool
}

func (c *CLI) serveConfig(ctx context.Context) (serveConfig, error) {
	var cfg serveConfig
	out, err := c.exec(ctx, nil, "serve", "status", "--json")
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return cfg, fmt.Errorf("tailscale serve status: unexpected output: %w", err)
	}
	return cfg, nil
}

// Publication is where Tailscale serves Artus.
type Publication struct {
	URL    string `json:"url"`    // https://vm.tail1234.ts.net, with a port when it is not 443
	Funnel bool   `json:"funnel"` // Tailscale Funnel also offers it to the public internet
}

// Published reports where target, the local address of Artus, is served on
// the tailnet, if it is.
func (c *CLI) Published(ctx context.Context, target string) (Publication, bool, error) {
	cfg, err := c.serveConfig(ctx)
	if err != nil {
		return Publication{}, false, err
	}
	p, ok := cfg.find(target)
	return p, ok, nil
}

func (cfg serveConfig) find(target string) (Publication, bool) {
	var hosts []string
	for hostport, web := range cfg.Web {
		if h, ok := web.Handlers["/"]; ok && sameTarget(h.Proxy, target) {
			hosts = append(hosts, hostport)
		}
	}
	if len(hosts) == 0 {
		return Publication{}, false
	}
	sort.Strings(hosts)
	return Publication{URL: httpsURL(hosts[0]), Funnel: cfg.AllowFunnel[hosts[0]]}, true
}

// sameTarget compares two proxy targets the way Tailscale stores them: a bare
// port and "localhost" both mean 127.0.0.1.
func sameTarget(a, b string) bool {
	norm := func(s string) string {
		if !strings.Contains(s, "://") {
			if !strings.Contains(s, ":") {
				s = "127.0.0.1:" + s
			}
			s = "http://" + s
		}
		u, err := url.Parse(s)
		if err != nil {
			return s
		}
		host := u.Hostname()
		if host == "localhost" {
			host = "127.0.0.1"
		}
		return u.Scheme + "://" + net.JoinHostPort(host, u.Port()) + strings.TrimSuffix(u.Path, "/")
	}
	return norm(a) == norm(b)
}

func httpsURL(hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "https://" + hostport
	}
	if port == "443" {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, port)
}

// ports are the ones Tailscale serves HTTPS on, in order of preference.
var ports = []string{"443", "8443", "10000"}

// Publish has Tailscale serve target on the tailnet over HTTPS and returns
// its address. It keeps what is already published and takes the first HTTPS
// port nothing else on this machine is served on. Tailscale may print a link
// on tty for the user to turn HTTPS on for their tailnet, and waits for them.
func (c *CLI) Publish(ctx context.Context, target string, tty *Terminal) (Publication, error) {
	cfg, err := c.serveConfig(ctx)
	if err != nil {
		return Publication{}, err
	}
	if p, ok := cfg.find(target); ok {
		return p, nil
	}
	port := ""
	for _, p := range ports {
		if _, taken := cfg.TCP[p]; !taken {
			port = p
			break
		}
	}
	if port == "" {
		return Publication{}, errors.New("Tailscale already serves something else on ports 443, 8443 and 10000 of this machine; free one with `tailscale serve --https=<port> off`")
	}
	if _, err := c.exec(ctx, tty, "serve", "--bg", "--https="+port, target); err != nil {
		return Publication{}, err
	}
	// The command also ends without an error when it only told the user how to
	// turn HTTPS on, so look at what is served now.
	if p, ok, err := c.Published(ctx, target); err != nil || ok {
		return p, err
	}
	return Publication{}, errors.New("Tailscale is not serving Artus yet. If it printed a link above, open it to turn on HTTPS for your tailnet, then run `artus setup` again")
}

// Unpublish stops serving at the address Publish returned.
func (c *CLI) Unpublish(ctx context.Context, address string, tty *Terminal) error {
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("not a Tailscale address: %q", address)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	_, err = c.exec(ctx, tty, "serve", "--https="+port, "off")
	return err
}
