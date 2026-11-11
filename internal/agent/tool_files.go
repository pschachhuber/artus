package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pschachhuber/artus/internal/llm"
	"github.com/pschachhuber/artus/internal/store"
)

var readTool = &Tool{
	Parallel: true,
	Def: llm.Tool{
		Name: "read",
		Description: "Read a file. Text comes back with line numbers; images and PDFs are shown to you directly; a directory lists its entries. " +
			"Relative paths start in the working directory (your home, unless a bash cd moved it). Large files: read a window with offset and limit, or grep first.",
		Schema: schema(`{"type":"object","properties":{
			"path":{"type":"string"},
			"offset":{"type":"integer","description":"First line to return (1-based)."},
			"limit":{"type":"integer","description":"Number of lines to return. Default 2000."}
		},"required":["path"]}`),
	},
	Run: runRead,
}

var mediaTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".pdf": "application/pdf",
}

func runRead(_ context.Context, c *Call) Result {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := c.args(&in); err != nil {
		return fail("%v", err)
	}
	path := c.path(in.Path)
	st, err := os.Stat(path)
	if err != nil {
		return fail("%s does not exist.", c.e.display(path))
	}
	if st.IsDir() {
		return listDir(c, path)
	}
	if rel, ok := c.e.Home.Rel(path); ok && rel == store.SecretsFile {
		return Result{Text: "Secret values are never shown. Names and Proton Pass references:\n\n" + c.e.Secrets.Masked()}
	}
	if mt, ok := mediaTypes[strings.ToLower(filepath.Ext(path))]; ok {
		if st.Size() > 20<<20 {
			return fail("%s is too large to show (%d MB).", c.e.display(path), st.Size()>>20)
		}
		if ok, why := attachable(path, mt); !ok {
			return fail("%s cannot be shown to you: %s. Inspect it with bash instead.", c.e.display(path), why)
		}
		kind := llm.Image
		if mt == "application/pdf" {
			kind = llm.File
		}
		return Result{
			Text:   fmt.Sprintf("%s (%s, %d bytes) is attached below.", c.e.display(path), mt, st.Size()),
			Blocks: []llm.Block{{Type: kind, Path: c.e.freeze(c.Chat.ID, path), MediaType: mt, Name: filepath.Base(path)}},
		}
	}
	if st.Size() > 50<<20 {
		return fail("%s is %d MB. Use grep, or bash tools such as head, tail and sed -n, to read part of it.", c.e.display(path), st.Size()>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fail("%v", err)
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return fail("%s is a binary file (%d bytes).", c.e.display(path), len(data))
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) == 0 {
		return Result{Text: "(the file is empty)"}
	}
	start := max(in.Offset, 1)
	if start > len(lines) {
		return fail("The file has only %d lines.", len(lines))
	}
	if in.Limit <= 0 {
		in.Limit = 2000
	}
	end := min(start-1+in.Limit, len(lines))
	var b strings.Builder
	for i := start - 1; i < end; i++ {
		line := lines[i]
		if r := []rune(line); len(r) > 2000 {
			line = string(r[:2000]) + fmt.Sprintf(" … [%d more characters]", len(r)-2000)
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "\n[lines %d-%d of %d; continue with offset %d]", start, end, len(lines), end+1)
	}
	return Result{Text: b.String()}
}

func listDir(c *Call, dir string) Result {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fail("%v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var b strings.Builder
	fmt.Fprintf(&b, "%s/ (%d entries)\n", c.e.display(dir), len(entries))
	for i, e := range entries {
		if i == 500 {
			fmt.Fprintf(&b, "… %d more\n", len(entries)-500)
			break
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		} else if info, err := e.Info(); err == nil {
			name += fmt.Sprintf("  (%d bytes)", info.Size())
		}
		b.WriteString(name + "\n")
	}
	return Result{Text: b.String()}
}

var writeTool = &Tool{
	Risky: true,
	Def: llm.Tool{
		Name:        "write",
		Description: "Create a file or replace its whole content. Parent folders are created. To change part of an existing file, use edit.",
		Schema:      schema(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
	},
	Run: func(_ context.Context, c *Call) Result {
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := c.args(&in); err != nil {
			return fail("%v", err)
		}
		path := c.path(in.Path)
		if res, locked := refuseLocked(c, path); locked {
			return res
		}
		_, statErr := os.Stat(path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fail("%v", err)
		}
		if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
			return fail("%v", err)
		}
		verb := "Created"
		if statErr == nil {
			verb = "Replaced"
		}
		note := ""
		if rel, ok := c.e.Home.Rel(path); ok && strings.HasPrefix(rel, "tools/") && strings.HasSuffix(rel, "/tool.json") {
			var t store.ExternalTool
			err := json.Unmarshal([]byte(in.Content), &t)
			if err == nil {
				err = store.CheckTool(t)
			}
			if err != nil {
				note = " This tool will not be offered until it is fixed: " + err.Error() + "."
			}
		}
		return Result{
			Text: fmt.Sprintf("%s %s (%d lines).%s", verb, c.e.display(path), strings.Count(in.Content, "\n")+1, note),
			UI:   map[string]any{"path": c.e.display(path), "new": preview(in.Content)},
		}
	},
}

var editTool = &Tool{
	Risky: true,
	Def: llm.Tool{
		Name: "edit",
		Description: "Replace an exact piece of text in a file. old_string must match the file exactly, whitespace included, and identify one place: " +
			"include enough surrounding lines to be unique, or set replace_all to change every occurrence.",
		Schema: schema(`{"type":"object","properties":{
			"path":{"type":"string"},
			"old_string":{"type":"string"},
			"new_string":{"type":"string"},
			"replace_all":{"type":"boolean"}
		},"required":["path","old_string","new_string"]}`),
	},
	Run: func(_ context.Context, c *Call) Result {
		var in struct {
			Path       string `json:"path"`
			Old        string `json:"old_string"`
			New        string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		}
		if err := c.args(&in); err != nil {
			return fail("%v", err)
		}
		path := c.path(in.Path)
		if res, locked := refuseLocked(c, path); locked {
			return res
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fail("%s does not exist. Use write to create it.", c.e.display(path))
		}
		text := string(data)
		if in.Old == "" {
			return fail("old_string is empty. Use write to create or replace a whole file.")
		}
		if in.Old == in.New {
			return fail("old_string and new_string are identical.")
		}
		crlf := strings.Contains(text, "\r\n") && !strings.Contains(in.Old, "\r\n")
		if crlf { // match against Unix line endings, write back what the file uses
			text = strings.ReplaceAll(text, "\r\n", "\n")
		}
		switch n := strings.Count(text, in.Old); {
		case n == 0:
			return fail("old_string was not found in %s. Read the file and copy the text exactly, including indentation.", c.e.display(path))
		case n > 1 && !in.ReplaceAll:
			return fail("old_string occurs %d times in %s. Add surrounding lines to pick one, or set replace_all.", n, c.e.display(path))
		}
		count := 1
		if in.ReplaceAll {
			count = strings.Count(text, in.Old)
			text = strings.ReplaceAll(text, in.Old, in.New)
		} else {
			text = strings.Replace(text, in.Old, in.New, 1)
		}
		if crlf {
			text = strings.ReplaceAll(text, "\n", "\r\n")
		}
		st, _ := os.Stat(path)
		if err := os.WriteFile(path, []byte(text), st.Mode().Perm()); err != nil {
			return fail("%v", err)
		}
		return Result{
			Text: fmt.Sprintf("Edited %s (%d replacement%s).", c.e.display(path), count, plural(count)),
			UI:   map[string]any{"path": c.e.display(path), "old": preview(in.Old), "new": preview(in.New)},
		}
	},
}

// refuseLocked explains why a user-owned file cannot be written.
func refuseLocked(c *Call, path string) (Result, bool) {
	if !c.e.Home.Locked(path) {
		return Result{}, false
	}
	rel, _ := c.e.Home.Rel(path)
	hint := "It belongs to the user, who changes it in Settings. Tell them what should change and why."
	switch {
	case rel == store.SecretsFile:
		hint = "Secrets are added with ask_secret (the user types the value) or `artus secret add NAME` (for a value you generated), and changed only by the user."
	case rel == store.ScheduleFile:
		hint = "Use the schedule tool to add or cancel triggers."
	case strings.HasPrefix(rel, "chats/"):
		hint = "Transcripts are a read-only record."
	}
	return fail("%s is locked. %s", rel, hint), true
}

func preview(s string) string {
	if r := []rune(s); len(r) > 4000 {
		return string(r[:4000]) + "\n…"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
