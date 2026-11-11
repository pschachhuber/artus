//go:build !windows

package browser

import (
	"os"
	"os/exec"
	"syscall"
)

// isolateProcess puts the browser in its own process group so the whole tree
// of renderer and utility processes can be signalled together.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree forcibly terminates the browser and its children.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		cmd.Process.Kill()
	}
}

// runningAsRoot reports whether the sandbox must be disabled for Chromium to
// start at all: it refuses to run sandboxed as root.
func runningAsRoot() bool { return os.Geteuid() == 0 }
