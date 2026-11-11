package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/secrets"
	"github.com/pschachhuber/artus/internal/store"
)

var bashTool = &Tool{
	Risky: true,
	Def: llm.Tool{
		Name: "bash",
		Description: "Run a shell command and return its combined output. The working directory persists between calls (cd works). " +
			"Secrets are injected as environment variables when the command mentions their NAME (for example $GITHUB_TOKEN); never echo them. " +
			"Use this for git, package managers, scripts, `artus browser …`, and anything without a dedicated tool. " +
			"For a long-running process such as a server, set background to get control back at once.",
		Schema: schema(`{"type":"object","properties":{
			"command":{"type":"string","description":"The command line to run."},
			"timeout":{"type":"integer","description":"Seconds before the command is stopped. Default 120, maximum 1800."},
			"background":{"type":"boolean","description":"Start the command and return immediately with its process id and log file."}
		},"required":["command"]}`),
	},
	Run: runBash,
}

// shell returns the shell used for commands: bash everywhere (Git Bash on
// Windows), PowerShell only when no bash can be found.
func shell() (path string, args []string) {
	if p, err := exec.LookPath("bash"); err == nil && !strings.Contains(strings.ToLower(p), `system32`) {
		return p, []string{"-c"}
	}
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData") + `\Programs`} {
			if p := filepath.Join(root, "Git", "bin", "bash.exe"); root != "" && fileExists(p) {
				return p, []string{"-c"}
			}
		}
		return "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command"}
	}
	return "/bin/sh", []string{"-c"}
}

func shellName() string {
	p, _ := shell()
	return strings.TrimSuffix(filepath.Base(p), ".exe")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// commandEnv builds the environment for a command the agent runs: the
// process environment without the harness's own credentials, plus the
// secrets the command names and the handles `artus …` needs to call back.
func (e *Engine) commandEnv(ctx context.Context, chat store.ChatMeta, command string) (env, used []string, err error) {
	hidden := map[string]bool{"ARTUS_TOKEN": true}
	for _, name := range secrets.Reserved {
		hidden[name] = true
	}
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !hidden[name] {
			env = append(env, kv)
		}
	}
	reason := "Artus: " + chat.Title
	secretEnv, used, err := e.Secrets.Env(ctx, command, reason)
	if err != nil {
		return nil, nil, err
	}
	env = append(env, secretEnv...)
	if strings.Contains(command, "pass-cli") {
		if passEnv, err := e.Secrets.PassEnv(ctx, reason); err == nil {
			for _, kv := range passEnv {
				if !strings.HasPrefix(kv, secrets.PassTokenName+"=") { // the logged-in session is enough
					env = append(env, kv)
				}
			}
		}
	}
	env = append(env,
		"ARTUS_HOME="+e.Home.Dir,
		"ARTUS_URL="+e.ServerURL,
		"ARTUS_AGENT_TOKEN="+e.AgentToken,
		"ARTUS_CHAT="+chat.ID,
	)
	if e.BinDir != "" {
		env = append(env, "PATH="+e.BinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return env, used, nil
}

func runBash(ctx context.Context, c *Call) Result {
	var in struct {
		Command    string `json:"command"`
		Timeout    int    `json:"timeout"`
		Background bool   `json:"background"`
	}
	if err := c.args(&in); err != nil {
		return fail("%v", err)
	}
	if strings.TrimSpace(in.Command) == "" {
		return fail("command is empty")
	}
	if in.Timeout <= 0 {
		in.Timeout = 120
	}
	if in.Timeout > 1800 {
		in.Timeout = 1800
	}
	env, used, err := c.e.commandEnv(ctx, c.Chat, in.Command)
	if err != nil {
		return fail("%v", err)
	}
	sh, shArgs := shell()
	dir := c.e.cwd(c.Chat.ID)
	script := in.Command
	var cwdFile string
	if !strings.Contains(sh, "powershell") && !in.Background {
		// Remember where the command leaves the shell, even if it calls exit.
		cwdFile = c.e.Home.Path("tmp", "cwd-"+store.RandomID(4))
		env = append(env, "ARTUS_CWD_FILE="+cwdFile)
		script = `trap 'pwd > "$ARTUS_CWD_FILE"' EXIT` + "\n" + in.Command
		defer os.Remove(cwdFile)
	}

	if in.Background {
		logPath := c.e.Home.Path("tmp", "bg-"+store.RandomID(4)+".log")
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fail("%v", err)
		}
		defer logFile.Close()
		cmd := exec.Command(sh, append(shArgs, script)...)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, logFile, logFile
		detach(cmd)
		if err := cmd.Start(); err != nil {
			return fail("could not start: %v", err)
		}
		go cmd.Wait()
		return Result{Text: fmt.Sprintf("Started in the background: pid %d, output in %s", cmd.Process.Pid, c.e.display(logPath))}
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(in.Timeout)*time.Second)
	defer cancel()
	cmd := exec.Command(sh, append(shArgs, script)...)
	cmd.Dir, cmd.Env = dir, env
	detach(cmd)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return fail("could not start %s: %v", shellName(), err)
	}
	var out capped
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 8192)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				out.Write(buf[:n])
				c.output(c.e.Secrets.Redact(string(bytes.ToValidUTF8(buf[:n], nil))))
			}
			if err != nil {
				return
			}
		}
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-waited:
	case <-runCtx.Done():
		killTree(cmd)
		<-waited
		runErr = runCtx.Err()
	}
	pw.Close()
	wg.Wait()

	if cwdFile != "" {
		if b, err := os.ReadFile(cwdFile); err == nil {
			if next := nativePath(strings.TrimSpace(string(b))); next != "" {
				c.e.setCwd(c.Chat.ID, next)
			}
		}
	}
	text := strings.TrimRight(strings.ToValidUTF8(out.String(), "�"), "\n")
	res := Result{Text: text, UI: map[string]any{"secrets": used}}
	var exit *exec.ExitError
	switch {
	case errors.Is(runErr, context.DeadlineExceeded):
		res.Text += fmt.Sprintf("\n[stopped after the %ds timeout; pass a larger timeout or run it in the background]", in.Timeout)
		res.IsError = true
	case errors.Is(runErr, context.Canceled):
		res.Text += "\n[stopped by the user]"
		res.IsError = true
	case errors.As(runErr, &exit):
		res.Text += fmt.Sprintf("\n[exit code %d]", exit.ExitCode())
		res.IsError = true
	case runErr != nil:
		res.Text += "\n[" + runErr.Error() + "]"
		res.IsError = true
	}
	if strings.TrimSpace(res.Text) == "" {
		res.Text = "(no output)"
	}
	return res
}

// nativePath converts the MSYS path Git Bash prints (/c/Users/x) to a
// Windows path. Elsewhere it returns p unchanged.
func nativePath(p string) string {
	if runtime.GOOS != "windows" || len(p) < 3 || p[0] != '/' || p[2] != '/' {
		return p
	}
	return strings.ToUpper(p[1:2]) + ":" + filepath.FromSlash(p[2:])
}

// capped collects output but keeps only the head and tail of very large
// streams so a chatty command cannot exhaust memory.
type capped struct {
	mu         sync.Mutex
	head, tail []byte
	dropped    int
}

const cappedPart = 2 << 20

func (c *capped) Write(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := cappedPart - len(c.head); room > 0 {
		n := min(room, len(p))
		c.head = append(c.head, p[:n]...)
		p = p[n:]
	}
	c.tail = append(c.tail, p...)
	if over := len(c.tail) - cappedPart; over > 0 {
		c.dropped += over
		c.tail = c.tail[over:]
	}
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == 0 {
		return string(c.head) + string(c.tail)
	}
	return fmt.Sprintf("%s\n… %d bytes dropped …\n%s", c.head, c.dropped, c.tail)
}
