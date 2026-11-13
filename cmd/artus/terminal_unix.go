//go:build !windows

package main

import "os"

// terminal reports whether someone can type into f. The null device is a
// character device too, and what a script or a service gets for its input.
func terminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(info, null)
}
