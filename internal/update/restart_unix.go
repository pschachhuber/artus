//go:build !windows

package update

import (
	"os"
	"syscall"
)

// Restart replaces this process with the program at exe, keeping the
// arguments, the terminal and the process id. It returns only on failure.
func Restart(exe string, env []string) error {
	return syscall.Exec(exe, os.Args, env)
}
