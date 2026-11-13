package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pschachhuber/artus/internal/store"
	"github.com/pschachhuber/artus/internal/tailscale"
)

// setup makes this machine an Artus server: Tailscale publishes Artus on the
// user's tailnet, the Tailscale account that owns the machine is signed in
// there without a link, and Artus starts when the machine does. Every step
// can be run again; it changes only what is not yet as it should be.
type setup struct {
	out         io.Writer
	in          *bufio.Reader
	tty         *tailscale.Terminal
	interactive bool // someone is at the terminal to answer
	yes         bool // answer every question with yes
	goos        string
	root        bool
	toldSudo    bool // said once why sudo comes up

	tailscale  func() (*tailscale.CLI, bool)
	run        func(name string, args ...string) error // on the user's terminal
	service    *service
	config     func() (store.Config, error)
	saveRemote func(store.Remote) error
	running    func() (pid int, managed, ok bool) // the Artus serving this home, if any
	stop       func(pid int) error
}

func setupCommand(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	yes := fs.Bool("yes", false, "answer every question with yes")
	remove := fs.Bool("remove", false, "undo what setup did")
	var users userList
	fs.Var(&users, "user", "also let this Tailscale account in")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("setup takes no arguments, got %q", fs.Arg(0))
	}
	if os.Getenv("ARTUS_AGENT_TOKEN") != "" {
		return errors.New("setup decides who may use Artus, which is the user's call; ask them to run it")
	}
	dir := store.DefaultDir()
	svc, err := detectService()
	if err != nil {
		return err
	}
	s := &setup{
		out: os.Stdout, in: bufio.NewReader(os.Stdin),
		tty:         &tailscale.Terminal{In: os.Stdin, Out: os.Stdout, Err: os.Stderr},
		interactive: terminal(os.Stdin),
		yes:         *yes, goos: runtime.GOOS, root: svc.Root,
		tailscale: tailscale.Find, run: runAttached, service: svc,
		config:     func() (store.Config, error) { return savedConfig(dir) },
		saveRemote: func(r store.Remote) error { return saveRemote(dir, r) },
		running:    func() (int, bool, bool) { return runningArtus(dir) },
		stop: func(pid int) error {
			p, err := os.FindProcess(pid)
			if err != nil {
				return err
			}
			return p.Signal(syscall.SIGTERM)
		},
	}
	if *remove {
		return s.undo()
	}
	return s.do(users)
}

// terminal reports whether someone can type into f. The null device is a
// character device too, and what a script or a service gets for its input.
func terminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(info, null)
}

// userList collects --user flags; one flag may also hold several accounts.
type userList []string

func (u *userList) String() string { return strings.Join(*u, ",") }
func (u *userList) Set(v string) error {
	*u = append(*u, strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })...)
	return nil
}

func (s *setup) say(format string, args ...any)  { fmt.Fprintf(s.out, "  "+format+"\n", args...) }
func (s *setup) done(format string, args ...any) { fmt.Fprintf(s.out, "  ✓ "+format+"\n", args...) }
func (s *setup) head(title string)               { fmt.Fprintf(s.out, "\n%s\n", title) }

// ask puts a yes-or-no question. With nobody to answer, the answer is no,
// and the caller says what to do by hand.
func (s *setup) ask(question string) bool {
	if s.yes {
		return true
	}
	if !s.interactive {
		return false
	}
	fmt.Fprintf(s.out, "  %s [Y/n] ", question)
	line, err := s.in.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if err != nil && answer == "" { // the input ended: nobody pressed Enter
		fmt.Fprintln(s.out)
		return false
	}
	return answer == "" || answer == "y" || answer == "yes"
}

func (s *setup) do(users []string) error {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	fmt.Fprintln(s.out, "Artus setup")

	s.head("Reach Artus from your other devices, through Tailscale")
	remote, err := s.publish(localAddress(cfg.Server), cfg.Remote, users)
	if err != nil {
		return err
	}

	s.head("Keep Artus running")
	if err := s.startAtBoot(); err != nil {
		return err
	}

	s.head("Done")
	if remote.URL == "" {
		s.say("Artus is not reachable from other devices yet. Run `artus setup` again once Tailscale is ready.")
	} else {
		s.say("Open  %s", remote.URL)
		if len(remote.Users) > 0 {
			s.say("from any device signed in to Tailscale as %s. There is nothing else to sign in to.", strings.Join(remote.Users, " or "))
			s.say("On a phone: install the Tailscale app, sign in, open the address, and add it to the home screen.")
		} else {
			s.say("No Tailscale account is let in yet: run `artus setup --user you@example.com`.")
		}
	}
	s.say("On this machine, `artus url` prints a link that opens Artus.")
	return nil
}

// localAddress is where Artus listens on this machine, which is what
// Tailscale forwards to.
func localAddress(cfg store.Server) string {
	return "http://" + net.JoinHostPort(displayHost(cfg.Host), fmt.Sprint(cfg.Port))
}

// publish has Tailscale serve Artus and records the address and the accounts
// that may use it. It returns what is recorded, also when a step was left
// for later.
func (s *setup) publish(target string, have store.Remote, extra []string) (store.Remote, error) {
	ctx := context.Background()
	cli, ok := s.tailscale()
	if !ok {
		if !s.installTailscale() {
			return have, nil
		}
		if cli, ok = s.tailscale(); !ok {
			return have, errors.New("Tailscale was installed but its command line client was not found; open a new terminal and run `artus setup` again")
		}
	}
	st, err := cli.Status(ctx)
	if err != nil {
		return have, fmt.Errorf("%w (is Tailscale running?)", err)
	}
	if !st.Connected() {
		s.say("Tailscale is installed, but this machine is not connected to your tailnet.")
		if !s.ask("Connect it now? Tailscale shows a link to sign in with.") {
			s.say("Connect it with `sudo tailscale up`, then run `artus setup` again.")
			return have, nil
		}
		if err := s.elevated(cli, func(c *tailscale.CLI) error { return c.Up(ctx, s.tty) }); err != nil {
			return have, err
		}
		if st, err = cli.Status(ctx); err != nil || !st.Connected() {
			return have, errors.New("this machine is still not connected to a tailnet; run `sudo tailscale up` and then `artus setup` again")
		}
	}
	s.done("Tailscale is connected: this machine is %s", st.Name)

	var pub tailscale.Publication
	err = s.elevated(cli, func(c *tailscale.CLI) error {
		var err error
		pub, err = c.Publish(ctx, target, s.tty)
		return err
	})
	if err != nil {
		return have, err
	}
	s.done("Artus is served at %s, to your tailnet only", pub.URL)
	if pub.Funnel {
		s.say("Warning: Tailscale Funnel is on for that address. Artus refuses what comes from the public internet; turn Funnel off with `tailscale funnel reset`.")
	}

	users := append(append(append([]string{}, have.Users...), extra...), st.User)
	if st.User == "" && len(have.Users)+len(extra) == 0 {
		s.say("This machine belongs to a tag, not to a person, so Tailscale cannot say who its owner is.")
		if s.interactive && !s.yes {
			fmt.Fprint(s.out, "  Which Tailscale account may use Artus (the email you sign in to Tailscale with)? ")
			line, _ := s.in.ReadString('\n')
			users = append(users, strings.TrimSpace(line))
		}
	}
	remote, err := store.Remote{URL: pub.URL, Users: users}.Clean()
	if err != nil {
		return have, err
	}
	if err := s.saveRemote(remote); err != nil {
		return have, err
	}
	if len(remote.Users) > 0 {
		s.done("Signed in there without a link: %s", strings.Join(remote.Users, ", "))
	}
	return remote, nil
}

// elevated runs fn with the client, and again through sudo when Tailscale
// lets only root do what it asks.
func (s *setup) elevated(cli *tailscale.CLI, fn func(*tailscale.CLI) error) error {
	err := fn(cli)
	if errors.Is(err, tailscale.ErrDenied) && !s.root && s.goos != "windows" {
		if !s.toldSudo {
			s.say("Tailscale lets only root change its settings, so sudo may ask for your password.")
			s.toldSudo = true
		}
		err = fn(cli.AsRoot())
	}
	if errors.Is(err, tailscale.ErrDenied) && s.goos == "windows" {
		return errors.New("Tailscale lets only an administrator change its settings; run `artus setup` from a terminal opened as Administrator")
	}
	return err
}

// installTailscale offers to run Tailscale's installer and reports whether
// it did.
func (s *setup) installTailscale() bool {
	const script = "curl -fsSL https://tailscale.com/install.sh | sh"
	// The same, fetched to a file first: in a pipe, a download that fails still ends well.
	const checked = `f=$(mktemp) && curl -fsSL https://tailscale.com/install.sh -o "$f" && sh "$f"; s=$?; rm -f "$f"; exit $s`
	if s.goos != "linux" {
		s.say("Tailscale is not installed. Get it from https://tailscale.com/download, sign in, and run `artus setup` again.")
		return false
	}
	s.say("Tailscale is not installed. It is a separate program that connects your devices privately.")
	if !s.ask("Install it now with Tailscale's own installer? It asks for sudo.") {
		s.say("To install it yourself:  %s", script)
		s.say("Then run `artus setup` again.")
		return false
	}
	if err := s.run("sh", "-c", checked); err != nil {
		s.say("Tailscale's installer did not finish (%v). Install it by hand, then run `artus setup` again.", err)
		return false
	}
	return true
}

// startAtBoot installs the service and moves a hand-started Artus under it.
func (s *setup) startAtBoot() error {
	svc := s.service
	if svc == nil || svc.Kind == "" {
		s.say("Artus does not know this system's service manager. To keep it running, have `artus serve --no-open` started when the machine starts.")
		return nil
	}
	when := "when this machine starts"
	if svc.Kind == "launchd" {
		when = "when you log in to this Mac"
	}
	if svc.Kind == "systemd" && svc.User == "root" {
		s.say("Note: you are root, so Artus and every command the agent runs would be root too. A user of its own is safer.")
	}
	question := "Start Artus " + when + "?"
	if svc.Kind == "systemd" && !svc.Root {
		question += " It asks for sudo."
	}
	if !s.ask(question) {
		s.say("Not installed as a service. Artus runs while `artus` is open in a terminal.")
		return nil
	}
	if pid, managed, ok := s.running(); ok && !managed {
		if !s.ask(fmt.Sprintf("Artus is running in a terminal (process %d). Stop it so that the service takes over?", pid)) {
			s.say("Left as it is. Stop that Artus and run `artus setup` again to move it under the service.")
			return nil
		}
		if err := s.stop(pid); err != nil {
			return fmt.Errorf("could not stop the running Artus: %w", err)
		}
		if !s.wait(func() bool { _, _, ok := s.running(); return !ok }) {
			return errors.New("the running Artus did not stop; stop it and run `artus setup` again")
		}
	}
	if err := svc.install(); err != nil {
		return err
	}
	if !s.wait(func() bool { _, managed, ok := s.running(); return ok && managed }) {
		hint := "journalctl -u artus -n 50"
		if svc.Kind == "launchd" {
			hint = "~/Library/Logs/artus.log"
		}
		return fmt.Errorf("the service is installed (%s) but Artus did not come up; see %s", svc.Path, hint)
	}
	s.done("Artus starts %s, and is running now", when)
	return nil
}

// wait polls until ready, for up to 20 seconds.
func (s *setup) wait(ready func() bool) bool {
	for range 80 {
		if ready() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return ready()
}

// undo takes Artus off the tailnet and removes the service.
func (s *setup) undo() error {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	fmt.Fprintln(s.out, "Artus setup: removing")
	if cfg.Remote.URL != "" {
		if cli, ok := s.tailscale(); ok {
			err := s.elevated(cli, func(c *tailscale.CLI) error { return c.Unpublish(context.Background(), cfg.Remote.URL, s.tty) })
			if err != nil {
				s.say("Tailscale still serves %s (%v). Stop it with `tailscale serve reset`.", cfg.Remote.URL, err)
			}
		}
		if err := s.saveRemote(store.Remote{}); err != nil {
			return err
		}
		s.done("Artus is no longer reachable through Tailscale")
	}
	if svc := s.service; svc != nil && svc.Kind != "" && svc.installed() {
		if err := svc.remove(); err != nil {
			return err
		}
		s.done("Artus no longer starts by itself, and was stopped")
	}
	s.say("Your data in %s is untouched.", store.DefaultDir())
	return nil
}

// savedConfig reads the configuration without opening the home next to a
// running Artus, which would start a second set of books there.
func savedConfig(dir string) (store.Config, error) {
	if _, ok := liveServer(dir); !ok {
		home, err := store.Open(dir, nil) // nothing is running: set up the home if needed
		if err != nil {
			return store.Config{}, err
		}
		return home.Config(), nil
	}
	var cfg store.Config
	b, err := os.ReadFile(filepath.Join(dir, store.ConfigFile))
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(b, &cfg)
}

// saveRemote records the remote address and accounts: through the running
// Artus, which would otherwise put its own copy of config.json back, or in
// the file when none is running.
func saveRemote(dir string, remote store.Remote) error {
	if url, ok := liveServer(dir); ok {
		base, token, _ := strings.Cut(url, "/?token=")
		return client{base, token}.call("PUT", "/api/remote", remote, nil)
	}
	home, err := store.Open(dir, nil)
	if err != nil {
		return err
	}
	return home.UpdateConfig(func(c *store.Config) { c.Remote = remote })
}

// runningArtus reports the process serving this home and whether a service
// manager started it.
func runningArtus(dir string) (pid int, managed, ok bool) {
	if _, live := liveServer(dir); !live {
		return 0, false, false
	}
	var state struct {
		PID     int  `json:"pid"`
		Managed bool `json:"managed"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, stateFile))
	json.Unmarshal(b, &state)
	return state.PID, state.Managed, true
}

// managed reports whether the service setup installed started this process.
func managed() bool { return os.Getenv(serviceEnv) != "" }
