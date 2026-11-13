package main

import (
	"os"
	"syscall"
)

// terminal reports whether someone can type into f: only a console has a
// console mode. NUL, a pipe and a file have none.
func terminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}
