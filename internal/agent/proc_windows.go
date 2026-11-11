//go:build windows

package agent

import (
	"os/exec"
	"strconv"
	"syscall"
)

// detach starts the command in a new process group and without a console window.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
}

// killTree stops the command and everything it started.
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() != nil {
		cmd.Process.Kill()
	}
}
