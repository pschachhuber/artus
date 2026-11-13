package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fake stands in for the Tailscale client: it keeps a serve configuration and
// answers the commands Artus runs in the format the real client prints.
type fake struct {
	status string
	serve  serveConfig
	https  bool // whether the tailnet has HTTPS turned on
	denied bool // whether only root may change what is served
	ran    []string
}

const connected = `{
  "Version": "1.104.7-t0123456789",
  "BackendState": "Running",
  "Self": {"DNSName": "VM.tail1234.ts.net.", "UserID": 8112345678901234567, "HostName": "vm"},
  "CertDomains": ["vm.tail1234.ts.net"],
  "User": {"8112345678901234567": {"ID": 8112345678901234567, "LoginName": "paul@example.com", "DisplayName": "Paul"}}
}`

func (f *fake) cli() *CLI {
	return &CLI{Path: "tailscale", Run: func(_ context.Context, tty *Terminal, argv ...string) ([]byte, error) {
		root := argv[0] == "sudo"
		if root {
			argv = argv[1:]
		}
		args := argv[1:]
		line := strings.Join(args, " ")
		f.ran = append(f.ran, strings.Join(argv, " "))
		if root {
			f.ran[len(f.ran)-1] = "sudo " + f.ran[len(f.ran)-1]
		}
		switch {
		case line == "status --json --peers=false":
			return []byte(f.status), nil
		case line == "serve status --json":
			return json.MarshalIndent(f.serve, "", "  ")
		case len(args) == 4 && args[0] == "serve" && args[1] == "--bg" && strings.HasPrefix(args[2], "--https="):
			if f.denied && !root {
				return nil, errors.New("Access denied: serve config denied\n\nUse 'sudo tailscale serve --bg'.")
			}
			if !f.https { // the real client prints where to turn HTTPS on and ends without an error
				fmt.Fprintln(tty.Out, "Serve is not enabled on your tailnet. To enable, visit: https://login.tailscale.com/f/serve")
				return nil, nil
			}
			port := strings.TrimPrefix(args[2], "--https=")
			if f.serve.TCP == nil {
				f.serve.TCP = map[string]json.RawMessage{}
				f.serve.Web = map[string]struct {
					Handlers map[string]struct{ Proxy string }
				}{}
			}
			f.serve.TCP[port] = json.RawMessage(`{"HTTPS":true}`)
			web := f.serve.Web["vm.tail1234.ts.net:"+port]
			web.Handlers = map[string]struct{ Proxy string }{"/": {Proxy: args[3]}}
			f.serve.Web["vm.tail1234.ts.net:"+port] = web
			return nil, nil
		case len(args) == 3 && args[0] == "serve" && args[2] == "off":
			port := strings.TrimPrefix(args[1], "--https=")
			delete(f.serve.TCP, port)
			delete(f.serve.Web, "vm.tail1234.ts.net:"+port)
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected command: tailscale %s", line)
	}}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	f := &fake{status: connected}
	st, err := f.cli().Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Status{Version: "1.104.7-t0123456789", State: "Running", Name: "vm.tail1234.ts.net", User: "paul@example.com", HTTPS: true}
	if st != want || !st.Connected() {
		t.Errorf("status = %+v, want %+v", st, want)
	}

	// What a freshly installed, logged-out Tailscale 1.104 prints.
	f.status = `{"Version":"1.104.7","BackendState":"NeedsLogin","AuthURL":"","Self":{"DNSName":"","UserID":0,"HostName":"vm"},"CertDomains":null,"User":null}`
	st, err = f.cli().Status(ctx)
	if err != nil || st.Connected() || st.State != "NeedsLogin" || st.User != "" {
		t.Errorf("logged out: %+v, %v", st, err)
	}

	// A machine that belongs to a tag has no person to sign in as.
	f.status = `{"BackendState":"Running","Self":{"DNSName":"vm.tail1234.ts.net.","UserID":42,"Tags":["tag:server"]},"User":{"42":{"LoginName":"tagged-devices"}}}`
	st, _ = f.cli().Status(ctx)
	if !st.Tagged || st.User != "" || !st.Connected() {
		t.Errorf("tagged machine: %+v", st)
	}

	f.status = "Logged out."
	if _, err := f.cli().Status(ctx); err == nil {
		t.Error("output that is not JSON was accepted")
	}
}

func TestPublish(t *testing.T) {
	ctx := context.Background()
	const target = "http://127.0.0.1:7717"
	f := &fake{status: connected, https: true}
	c := f.cli()
	var out strings.Builder
	tty := &Terminal{Out: &out, Err: &out}

	if _, ok, err := c.Published(ctx, target); ok || err != nil {
		t.Fatalf("published before anything was: %v %v", ok, err)
	}
	p, err := c.Publish(ctx, target, tty)
	if err != nil || p.URL != "https://vm.tail1234.ts.net" || p.Funnel {
		t.Fatalf("publish: %+v, %v", p, err)
	}
	// A second run finds it and changes nothing.
	before := len(f.ran)
	if p, err = c.Publish(ctx, target, tty); err != nil || p.URL != "https://vm.tail1234.ts.net" {
		t.Fatalf("publish again: %+v, %v", p, err)
	}
	for _, line := range f.ran[before:] {
		if strings.Contains(line, "serve --bg") {
			t.Errorf("served again although it was published: %s", line)
		}
	}
	// Tailscale stores a bare port as this address; both name the same Artus.
	if _, ok, _ := c.Published(ctx, "7717"); !ok {
		t.Error("a bare port was not recognised as the same target")
	}
	if _, ok, _ := c.Published(ctx, "http://127.0.0.1:7718"); ok {
		t.Error("another port was taken for Artus")
	}

	f.serve.AllowFunnel = map[string]bool{"vm.tail1234.ts.net:443": true}
	if p, _, _ = c.Published(ctx, target); !p.Funnel {
		t.Error("Funnel on the address went unnoticed")
	}

	if err := c.Unpublish(ctx, p.URL, tty); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Published(ctx, target); ok {
		t.Error("still published after unpublishing")
	}
}

func TestPublishBesideOtherServices(t *testing.T) {
	ctx := context.Background()
	f := &fake{status: connected, https: true}
	c := f.cli()
	tty := &Terminal{}
	// Something else already has 443 on this machine: Artus leaves it alone.
	if _, err := c.Publish(ctx, "http://127.0.0.1:3000", tty); err != nil {
		t.Fatal(err)
	}
	p, err := c.Publish(ctx, "http://127.0.0.1:7717", tty)
	if err != nil || p.URL != "https://vm.tail1234.ts.net:8443" {
		t.Fatalf("second service: %+v, %v", p, err)
	}
	if other, ok, _ := c.Published(ctx, "http://127.0.0.1:3000"); !ok || other.URL != "https://vm.tail1234.ts.net" {
		t.Errorf("the other service was disturbed: %+v %v", other, ok)
	}
	if err := c.Unpublish(ctx, p.URL, tty); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Published(ctx, "http://127.0.0.1:3000"); !ok {
		t.Error("unpublishing Artus removed the other service")
	}
	if got := f.ran[len(f.ran)-2]; got != "tailscale serve --https=8443 off" {
		t.Errorf("unpublish ran %q", got)
	}
}

func TestPublishFailures(t *testing.T) {
	ctx := context.Background()
	var out strings.Builder
	tty := &Terminal{Out: &out, Err: &out}

	f := &fake{status: connected} // HTTPS not turned on for the tailnet
	_, err := f.cli().Publish(ctx, "http://127.0.0.1:7717", tty)
	if err == nil || !strings.Contains(err.Error(), "turn on HTTPS") {
		t.Errorf("HTTPS off: %v", err)
	}
	if !strings.Contains(out.String(), "https://login.tailscale.com/f/serve") {
		t.Errorf("the user never saw Tailscale's link: %q", out.String())
	}

	// Tailscale lets only root and its operator change what is served; sudo gets past that.
	f = &fake{status: connected, https: true, denied: true}
	if _, err := f.cli().Publish(ctx, "http://127.0.0.1:7717", tty); !errors.Is(err, ErrDenied) {
		t.Errorf("not the operator: %v", err)
	}
	if p, err := f.cli().AsRoot().Publish(ctx, "http://127.0.0.1:7717", tty); err != nil || p.URL == "" {
		t.Errorf("as root: %+v, %v", p, err)
	}
	if got := f.ran[len(f.ran)-2]; got != "sudo tailscale serve --bg --https=443 http://127.0.0.1:7717" {
		t.Errorf("as root ran %q", got)
	}

	f = &fake{status: connected, https: true}
	f.serve.TCP = map[string]json.RawMessage{"443": nil, "8443": nil, "10000": nil}
	if _, err := f.cli().Publish(ctx, "http://127.0.0.1:7717", tty); err == nil {
		t.Error("published although every port was taken")
	}
}
