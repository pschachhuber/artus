// Artus is a minimal personal agent harness: one binary that serves a local
// web interface, runs the agent loop, fires scheduled tasks and drives a
// browser.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pschachhuber/artus/internal/browser"
	"github.com/pschachhuber/artus/internal/store"
	"github.com/pschachhuber/artus/internal/update"
)

// version is set at build time.
var version = "dev"

const usage = `Artus — a minimal personal agent harness.

Usage:
  artus                      start Artus and open the interface
  artus serve [flags]        the same, with options
      --port N               listen on this port (default: from config, 7717)
      --no-open              do not open a browser window
  artus run [flags] "task"   run one prompt and print the answer
      --model provider/id    model to use (default: from config)
      --effort level         low | medium | high | xhigh | max
  artus url                  print the link that signs a browser in
  artus browser <command>    drive the browser (see: artus browser help)
  artus secret add NAME      store a secret read from standard input
  artus update [--check]     install the latest release (--check: only look)
  artus version

The data folder is ~/.artus, or $ARTUS_HOME.
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "run":
		err = runOnce(args)
	case "url":
		err = printURL()
	case "browser":
		err = browserCommand(args)
	case "secret":
		err = secretCommand(args)
	case "update":
		err = updateCommand(args)
	case "version", "--version", "-v":
		fmt.Println("artus", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "artus:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 0, "port to listen on")
	noOpen := fs.Bool("no-open", false, "do not open a browser window")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)
	updatedFrom := os.Getenv("ARTUS_UPDATED_FROM") // set when an update restarted Artus
	os.Unsetenv("ARTUS_UPDATED_FROM")
	if updatedFrom != "" {
		*noOpen = true // the open tab reconnects by itself
	}

	if url, ok := liveServer(store.DefaultDir()); ok {
		fmt.Println("Artus is already running:", url)
		if !*noOpen {
			openBrowser(url)
		}
		return nil
	}
	a, err := newApp(store.DefaultDir())
	if err != nil {
		return err
	}
	a.server.Updates = a.updates
	if updatedFrom != "" {
		a.updates.UpdatedFrom(updatedFrom)
	}
	url, err := a.listen(*port)
	if err != nil {
		return err
	}
	updates, stopUpdates := context.WithCancel(context.Background())
	defer stopUpdates()
	go a.updates.Run(updates)
	if updatedFrom != "" {
		fmt.Printf("Updated from %s.\n", updatedFrom)
		go a.updates.Check(updates) // so the interface shows a current check, not "never"
	}
	fmt.Printf("Artus %s\n  home  %s\n  open  %s\n", version, a.home.Dir, url)
	if a.home.Config().Model == "" {
		fmt.Println("  Add an API key and choose a model in Settings to get started.")
	}
	if !*noOpen {
		openBrowser(url)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\nStopping…")
	a.close()
	return nil
}

// runOnce sends one prompt and prints the final answer. It talks to a
// running Artus if there is one and starts its own otherwise.
func runOnce(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	model := fs.String("model", "", "provider/model")
	effort := fs.String("effort", "", "reasoning effort")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "-" || prompt == "" {
		b, _ := io.ReadAll(os.Stdin)
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		return errors.New(`nothing to run: artus run "what to do"`)
	}
	c, stop, err := connect(true)
	if err != nil {
		return err
	}
	defer stop()

	var chat struct{ ID string }
	if err := c.call("POST", "/api/chats", map[string]string{"model": *model, "effort": *effort}, &chat); err != nil {
		return err
	}
	if err := c.call("POST", "/api/chats/"+chat.ID+"/messages", map[string]string{"text": prompt}, nil); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "chat", chat.ID)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	for {
		select {
		case <-sig:
			c.call("POST", "/api/chats/"+chat.ID+"/stop", nil, nil)
			return errors.New("stopped")
		case <-time.After(400 * time.Millisecond):
		}
		var state struct {
			Live    *struct{} `json:"live"`
			Records []struct {
				T     string `json:"t"`
				Note  string `json:"note"`
				Level string `json:"level"`
				Msg   *struct {
					Role   string `json:"role"`
					Blocks []struct{ Type, Text string }
				} `json:"msg"`
			} `json:"records"`
		}
		if err := c.call("GET", "/api/chats/"+chat.ID, nil, &state); err != nil {
			return err
		}
		if state.Live != nil {
			continue
		}
		var answer, failure string
		for _, rec := range state.Records {
			if rec.Msg != nil && rec.Msg.Role == "assistant" {
				var parts []string
				for _, b := range rec.Msg.Blocks {
					if b.Type == "text" && b.Text != "" {
						parts = append(parts, b.Text)
					}
				}
				if len(parts) > 0 {
					answer = strings.Join(parts, "\n")
				}
			}
			if rec.T == "note" && rec.Level == "error" {
				failure = rec.Note
			}
		}
		if answer != "" {
			fmt.Println(answer)
		}
		if failure != "" {
			return errors.New(failure)
		}
		return nil
	}
}

func printURL() error {
	home, err := store.Open(store.DefaultDir(), nil)
	if err != nil {
		return err
	}
	cfg := home.Config().Server
	base := fmt.Sprintf("http://%s", net.JoinHostPort(displayHost(cfg.Host), fmt.Sprint(cfg.Port)))
	if url, ok := liveServer(home.Dir); ok {
		base = url[:strings.Index(url, "/?")]
	}
	fmt.Printf("%s/?token=%s\n", base, cfg.Token)
	return nil
}

// browserCommand forwards `artus browser …` to the running Artus, which owns
// the browser processes.
func browserCommand(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Print(browser.Help())
		return nil
	}
	c, stop, err := connect(false)
	if err != nil {
		return err
	}
	defer stop()
	cwd, _ := os.Getwd()
	var out struct {
		Text  string
		Files []string
		Error bool
	}
	if err := c.call("POST", "/api/agent/browser", map[string]any{"args": args, "cwd": cwd}, &out); err != nil {
		return err
	}
	fmt.Println(out.Text)
	if out.Error {
		os.Exit(1)
	}
	return nil
}

func secretCommand(args []string) error {
	if len(args) != 2 || args[0] != "add" {
		return errors.New("usage: some-command | artus secret add NAME")
	}
	value, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	if err != nil {
		return err
	}
	c, stop, err := connect(false)
	if err != nil {
		return err
	}
	defer stop()
	if err := c.call("POST", "/api/agent/secret", map[string]string{"name": args[1], "value": strings.TrimSpace(string(value))}, nil); err != nil {
		return err
	}
	fmt.Println("Stored", args[1])
	return nil
}

func displayHost(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1"
	}
	return host
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", url)
	}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

// shutdown gives in-flight requests a moment, then closes the server.
func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if srv.Shutdown(ctx) != nil {
		srv.Close()
	}
}

// updateCommand looks for a newer release and installs it. A running Artus
// is asked to do it, so that it restarts into the new version; otherwise the
// program file is replaced directly.
func updateCommand(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only report whether a newer version exists")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)

	if url, ok := liveServer(store.DefaultDir()); ok {
		base, token, _ := strings.Cut(url, "/?token=")
		c := client{base, token}
		var st update.Status
		if err := c.call("POST", "/api/update/check", nil, &st); err != nil {
			return err
		}
		if err := reportUpdate(st.Current, st.Latest, st.Available, st.Error); err != nil || !st.Available || *check {
			return err
		}
		if st.Blocked != "" {
			return errors.New(st.Blocked)
		}
		if err := c.call("POST", "/api/update/install", nil, nil); err != nil {
			return err
		}
		fmt.Printf("The running Artus is installing %s and will restart.\n", st.Latest.Version)
		return nil
	}

	u := update.New(version)
	ctx := context.Background()
	rel, err := u.Latest(ctx)
	if errors.Is(err, update.ErrNoRelease) {
		return reportUpdate(version, nil, false, "")
	}
	if err != nil {
		return err
	}
	if err := reportUpdate(version, &rel, u.Newer(rel), ""); err != nil || !u.Newer(rel) || *check {
		return err
	}
	if why := u.Blocked(); why != "" {
		return errors.New(why)
	}
	fmt.Println("Downloading and checking…")
	if err := u.Install(ctx, rel); err != nil {
		return err
	}
	fmt.Printf("Installed %s at %s.\n", rel.Version, u.Exe)
	return nil
}

func reportUpdate(current string, latest *update.Release, available bool, problem string) error {
	switch {
	case problem != "":
		return errors.New(problem)
	case latest == nil:
		fmt.Printf("Artus %s. No release has been published yet.\n", current)
	case available:
		fmt.Printf("Artus %s. Version %s is available: %s\n", current, latest.Version, latest.URL)
	default:
		fmt.Printf("Artus %s is up to date (latest release: %s).\n", current, latest.Version)
	}
	return nil
}
