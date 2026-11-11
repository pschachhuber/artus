// Package web holds the interface: plain HTML, CSS and JavaScript, embedded
// in the binary and served as-is. There is no build step.
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html style.css *.js
var files embed.FS

// Files returns the interface's files.
func Files() fs.FS { return files }
