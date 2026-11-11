// Package defaults holds what a fresh Artus home starts with: the base system
// prompt and the skills and task seeded on first run.
package defaults

import (
	"embed"
	"io/fs"
)

// Prompt is the base system prompt. Memory, the user's instructions and the
// topic and skill indexes are appended to it for each conversation.
//
//go:embed prompt.md
var Prompt string

//go:embed all:home
var home embed.FS

// Home returns the files copied into a new data home. Existing files are
// never overwritten.
func Home() fs.FS {
	sub, err := fs.Sub(home, "home")
	if err != nil {
		panic(err)
	}
	return sub
}
