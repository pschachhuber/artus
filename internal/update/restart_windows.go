//go:build windows

package update

import (
	"os"
	"os/exec"
)

// Restart starts the program at exe with the same arguments and console and
// ends this process. It returns only on failure.
func Restart(exe string, env []string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
