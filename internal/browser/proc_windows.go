//go:build windows

package browser

import (
	"os/exec"
	"strconv"
	"syscall"
)

// isolateProcess detaches the browser from our console's Ctrl+C group.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessTree forcibly terminates the browser and its children.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() != nil {
		cmd.Process.Kill()
	}
}

func runningAsRoot() bool { return false }
