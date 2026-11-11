package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

func (h *Home) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=Artus", "-c", "user.email=artus@localhost",
		"-c", "commit.gpgsign=false", "-c", "core.autocrlf=false",
	}, args...)...)
	cmd.Dir = h.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

func (h *Home) initHistory() {
	if _, err := exec.LookPath("git"); err != nil {
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
		changes = append(changes, Change{Hash: parts[0], Time: t, Message: parts[2], Files: strings.Fields(files)})
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
	if _, err := h.git("revert", "--no-edit", hash); err != nil {
		h.git("revert", "--abort")
		return fmt.Errorf("cannot revert cleanly, later changes touch the same lines: %w", err)
	}
	return nil
}
