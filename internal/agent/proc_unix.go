//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// detach puts the command in its own process group so the whole tree can be stopped.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree stops the command and everything it started.
func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
