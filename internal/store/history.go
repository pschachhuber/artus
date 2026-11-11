package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// The home is a git repository that tracks only what the agent may edit, so
// every change it makes can be reviewed and reverted. git is optional: without
// it the change log is simply empty.
const gitignore = `# Managed by Artus. Tracks only what the agent may change.
/*
!/.gitignore
!/memory.md
!/instructions.md
!/topics/
!/skills/
!/tools/
!/tasks/
!/browser/
/browser/*
!/browser/config.json
`

// Change is one recorded set of edits to the agent-editable files.
type Change struct {
	Hash    string    `json:"hash"`
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
	Files   []string  `json:"files"`
}

// gitConfig is the repository configuration Artus writes before every git
// call. The agent's shell can reach .git, and git runs what its
// configuration names (hooks, filters, diff drivers, fsmonitor); with a
// known configuration and hooks pointed nowhere, git runs nothing else.
const gitConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = false\n\tbare = false\n\tlogallrefupdates = true\n"

func (h *Home) git(args ...string) (string, error) {
	if !h.hasGit {
		return "", errors.New("git is not installed")
	}
	if _, err := os.Stat(h.Path(".git", "config")); err == nil {
		if err := os.WriteFile(h.Path(".git", "config"), []byte(gitConfig), 0o600); err != nil {
			return "", err
		}
	}
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=Artus", "-c", "user.email=artus@localhost",
		"-c", "commit.gpgsign=false", "-c", "core.autocrlf=false", "-c", "core.quotepath=false",
		"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false",
	}, args...)...)
	cmd.Dir = h.Dir
	cmd.Env = []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	for _, kv := range os.Environ() { // git needs PATH, HOME and the like, never credentials
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		if !strings.HasPrefix(upper, "GIT_") && !strings.Contains(upper, "KEY") && !strings.Contains(upper, "TOKEN") &&
			!strings.Contains(upper, "SECRET") && !strings.Contains(upper, "PASS") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

// gitUsable reports whether git can be run. On macOS without the developer
// tools /usr/bin/git is a stub that opens an install dialog; that is not git.
func gitUsable() bool {
	path, err := exec.LookPath("git")
	if err != nil {
		return false
	}
	if runtime.GOOS == "darwin" && path == "/usr/bin/git" {
		return exec.Command("xcode-select", "-p").Run() == nil
	}
	return true
}

func (h *Home) initHistory() {
	if h.hasGit = gitUsable(); !h.hasGit {
		return
	}
	if err := os.WriteFile(h.Path(".gitignore"), []byte(gitignore), 0o600); err != nil {
		return
	}
	if _, err := os.Stat(h.Path(".git")); err != nil {
		if _, err := h.git("init", "-q"); err != nil {
			return
		}
	}
	h.Snapshot("Initial state")
}

// Snapshot records the current state of the agent-editable files if anything
// changed, and reports whether it did.
func (h *Home) Snapshot(message string) bool {
	if _, err := h.git("add", "-A"); err != nil {
		return false
	}
	if _, err := h.git("diff", "--cached", "--quiet"); err == nil {
		return false
	}
	if message = strings.TrimSpace(message); message == "" {
		message = "Change"
	}
	_, err := h.git("commit", "-q", "-m", message)
	return err == nil
}

// Changes lists the most recent snapshots, newest first.
func (h *Home) Changes(limit int) []Change {
	out, err := h.git("log", fmt.Sprintf("-%d", limit), "--name-only", "--format=%x1e%H%x1f%cI%x1f%s")
	if err != nil {
		return nil
	}
	var changes []Change
	for _, block := range strings.Split(out, "\x1e")[1:] {
		head, files, _ := strings.Cut(block, "\n")
		parts := strings.SplitN(head, "\x1f", 3)
		if len(parts) != 3 {
			continue
		}
		t, _ := time.Parse(time.RFC3339, parts[1])
		c := Change{Hash: parts[0], Time: t, Message: parts[2]}
		for _, file := range strings.Split(files, "\n") {
			if file = strings.TrimSpace(file); file != "" {
				c.Files = append(c.Files, file)
			}
		}
		changes = append(changes, c)
	}
	return changes
}

// Diff returns the patch of one snapshot.
func (h *Home) Diff(hash string) (string, error) {
	if !ValidID(hash) {
		return "", errors.New("invalid change id")
	}
	return h.git("show", "--format=", "--no-color", hash)
}

// Revert undoes one snapshot by recording its inverse.
func (h *Home) Revert(hash string) error {
	if !ValidID(hash) {
		return errors.New("invalid change id")
	}
	h.Snapshot("Before revert")
	before, _ := os.ReadFile(h.Path(InstructionsFile))
	if _, err := h.git("revert", "--no-edit", hash); err != nil {
		h.git("revert", "--abort")
		return fmt.Errorf("cannot revert cleanly, later changes touch the same lines: %w", err)
	}
	// The user asked for this, so what it did to their instructions stands.
	if after, err := os.ReadFile(h.Path(InstructionsFile)); err == nil && !bytes.Equal(before, after) {
		h.mu.Lock()
		h.guard[InstructionsFile] = after
		h.mu.Unlock()
	}
	return nil
}
