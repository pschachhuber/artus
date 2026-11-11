package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pschachhuber/artus/internal/llm"

	"github.com/pschachhuber/artus/internal/store"
)

var globTool = &Tool{
	Parallel: true,
	Def: llm.Tool{
		Name:        "glob",
		Description: "Find files by name pattern, newest first. ** matches any number of folders, for example topics/**/*.md or **/*.{go,md}.",
		Schema: schema(`{"type":"object","properties":{
			"pattern":{"type":"string"},
			"path":{"type":"string","description":"Folder to search. Default: the working directory."}
		},"required":["pattern"]}`),
	},
	Run: func(ctx context.Context, c *Call) Result {
		var in struct{ Pattern, Path string }
		if err := c.args(&in); err != nil {
			return fail("%v", err)
		}
		root := c.path(in.Path)
		if filepath.IsAbs(in.Pattern) {
			root, in.Pattern = splitAbsPattern(in.Pattern)
		}
		type hit struct {
			path string
			mod  int64
		}
		var hits []hit
		walk(ctx, root, c.hidden, func(p, rel string, d fs.DirEntry) {
			if matchGlob(in.Pattern, rel) {
				if info, err := d.Info(); err == nil {
					hits = append(hits, hit{p, info.ModTime().UnixNano()})
				}
			}
		})
		if len(hits) == 0 {
			return Result{Text: "No files match."}
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })
		var b strings.Builder
		for i, h := range hits {
			if i == 200 {
				fmt.Fprintf(&b, "… %d more; narrow the pattern\n", len(hits)-200)
				break
			}
			b.WriteString(c.e.display(h.path) + "\n")
		}
		return Result{Text: b.String()}
	},
}

// splitAbsPattern separates the fixed leading folders of an absolute pattern
// from the part that contains wildcards.
func splitAbsPattern(pattern string) (root, rest string) {
	parts := strings.Split(filepath.ToSlash(pattern), "/")
	i := 0
	for i < len(parts)-1 && !strings.ContainsAny(parts[i], "*?[{") {
		i++
	}
	return filepath.FromSlash(strings.Join(parts[:i], "/") + "/"), strings.Join(parts[i:], "/")
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, ".venv": true, "__pycache__": true}

// walk visits every file under root, skipping version-control and dependency
// folders, the browser's profile data, symlinked directories, and whatever
// skip rejects.
func walk(ctx context.Context, root string, skip func(p string) bool, visit func(p, rel string, d fs.DirEntry)) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return fs.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[d.Name()] || strings.HasSuffix(rel, "browser/profiles") || strings.HasSuffix(rel, "browser/chrome") {
				return fs.SkipDir
			}
			return nil
		}
		if skip == nil || !skip(p) {
			visit(p, rel, d)
		}
		return nil
	})
}

// hidden reports files a search should pass over: this conversation's own
// transcript (it is already in context, and a search would find itself in it)
// and the bookkeeping file beside each transcript.
func (c *Call) hidden(p string) bool {
	rel, ok := c.e.Home.Rel(p)
	switch {
	case !ok:
		return false
	case rel == store.SecretsFile, rel == "pass-session", strings.HasPrefix(rel, "pass-session/"), strings.HasPrefix(rel, ".git/"):
		return true // secrets are used by name, never searched
	case !strings.HasPrefix(rel, "chats/"):
		return false
	}
	return strings.HasSuffix(rel, ".json") || rel == "chats/"+c.Chat.ID+".jsonl"
}

// matchGlob matches a slash-separated relative path against a pattern with
// **, *, ?, [classes] and {a,b} alternatives. A pattern without a slash
// matches the file name at any depth.
func matchGlob(pattern, rel string) bool {
	pattern = filepath.ToSlash(pattern)
	if open := strings.Index(pattern, "{"); open >= 0 {
		if end := strings.Index(pattern[open:], "}"); end > 0 {
			for _, alt := range strings.Split(pattern[open+1:open+end], ",") {
				if matchGlob(pattern[:open]+alt+pattern[open+end+1:], rel) {
					return true
				}
			}
			return false
		}
	}
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(rel))
		return ok
	}
	return matchParts(strings.Split(strings.TrimPrefix(pattern, "./"), "/"), strings.Split(rel, "/"))
}

func matchParts(pattern, parts []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(parts); i++ {
				if matchParts(pattern[1:], parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 {
			return false
		}
		if ok, _ := path.Match(pattern[0], parts[0]); !ok {
			return false
		}
		pattern, parts = pattern[1:], parts[1:]
	}
	return len(parts) == 0
}

var grepTool = &Tool{
	Parallel: true,
	Def: llm.Tool{
		Name: "grep",
		Description: "Search file contents with a regular expression (RE2 syntax). Searches the working directory unless path is given; " +
			"path may be one file. This is how you search topics, skills and past conversations: in chats/ it searches what was said and shows who said it " +
			"(the current conversation is skipped unless you name its file). The secrets file is never searched.",
		Schema: schema(`{"type":"object","properties":{
			"pattern":{"type":"string"},
			"path":{"type":"string","description":"File or folder to search."},
			"glob":{"type":"string","description":"Only files matching this pattern, for example *.md."},
			"ignore_case":{"type":"boolean"},
			"context":{"type":"integer","description":"Lines to show before and after each match."},
			"output":{"type":"string","enum":["content","files","count"],"description":"content (default) shows matching lines; files lists files with matches; count gives matches per file."},
			"limit":{"type":"integer","description":"Maximum lines of output. Default 200."}
		},"required":["pattern"]}`),
	},
	Run: runGrep,
}

func runGrep(ctx context.Context, c *Call) Result {
	var in struct {
		Pattern, Path, Glob, Output string
		IgnoreCase                  bool `json:"ignore_case"`
		Context, Limit              int
	}
	if err := c.args(&in); err != nil {
		return fail("%v", err)
	}
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return fail("Invalid regular expression: %v", err)
	}
	if in.Limit <= 0 {
		in.Limit = 200
	}
	in.Context = min(max(in.Context, 0), 20)
	root := c.path(in.Path)
	st, err := os.Stat(root)
	if err != nil {
		return fail("%s does not exist.", c.e.display(root))
	}

	var b strings.Builder
	lines, files := 0, 0
	search := func(p string) {
		if lines >= in.Limit {
			return
		}
		var matches []grepLine
		if rel, ok := c.e.Home.Rel(p); ok && strings.HasPrefix(rel, "chats/") && strings.HasSuffix(rel, ".jsonl") {
			matches = grepTranscript(p, re)
		} else {
			matches = grepFile(p, re, in.Context, in.Output == "content" || in.Output == "")
		}
		if len(matches) == 0 {
			return
		}
		files++
		name := c.e.display(p)
		switch in.Output {
		case "files":
			b.WriteString(name + "\n")
			lines++
		case "count":
			n := 0
			for _, m := range matches {
				if m.hit {
					n++
				}
			}
			fmt.Fprintf(&b, "%s: %d\n", name, n)
			lines++
		default:
			for _, m := range matches {
				if lines >= in.Limit {
					break
				}
				sep := ":"
				if !m.hit {
					sep = "-"
				}
				fmt.Fprintf(&b, "%s%s%d%s%s\n", name, sep, m.line, sep, m.text)
				lines++
			}
		}
	}
	if !st.IsDir() {
		search(root)
	} else {
		walk(ctx, root, c.hidden, func(p, rel string, _ fs.DirEntry) {
			if in.Glob == "" || matchGlob(in.Glob, rel) {
				search(p)
			}
		})
	}
	if files == 0 {
		return Result{Text: "No matches."}
	}
	if lines >= in.Limit {
		fmt.Fprintf(&b, "[output limit of %d lines reached; narrow the search or raise limit]\n", in.Limit)
	}
	return Result{Text: b.String()}
}

type grepLine struct {
	line int
	text string
	hit  bool
}

// grepFile returns the matching lines of one text file, with context lines
// when asked. Very long lines (transcripts are one JSON object per line) are
// cut to a window around the match.
func grepFile(p string, re *regexp.Regexp, context int, wantText bool) []grepLine {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	if head, _ := r.Peek(4096); bytes.IndexByte(head, 0) >= 0 {
		return nil
	}
	var out []grepLine
	var before []grepLine
	after, n := 0, 0
	for {
		raw, err := r.ReadBytes('\n')
		if len(raw) > 0 {
			n++
			line := bytes.TrimRight(raw, "\r\n")
			if loc := re.FindIndex(line); loc != nil {
				out = append(out, before...)
				before = before[:0]
				text := ""
				if wantText {
					text = window(line, loc)
				}
				out = append(out, grepLine{n, text, true})
				after = context
				if !wantText && context == 0 && len(out) > 100000 {
					return out
				}
			} else if after > 0 {
				out = append(out, grepLine{n, window(line, nil), false})
				after--
			} else if context > 0 {
				before = append(before, grepLine{n, window(line, nil), false})
				if len(before) > context {
					before = before[1:]
				}
			}
		}
		if err != nil {
			return out
		}
	}
}

// grepTranscript searches a conversation by what was said in it. Each record
// is one JSON line holding whole messages, so matching the raw line would
// return unreadable JSON and would also hit the stored system prompt. Instead
// every message part is searched on its own and shown as "[who] text".
func grepTranscript(p string, re *regexp.Regexp) []grepLine {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	var out []grepLine
	for n := 1; ; n++ {
		raw, err := r.ReadBytes('\n')
		var rec Record
		if len(raw) > 0 && json.Unmarshal(raw, &rec) == nil {
			day := rec.Time.Local().Format("2006-01-02")
			add := func(who, text string) {
				if loc := re.FindStringIndex(text); loc != nil {
					out = append(out, grepLine{n, "[" + day + " " + who + "] " + window([]byte(strings.ReplaceAll(text, "\n", " ⏎ ")), shifted(loc, text)), true})
				}
			}
			switch {
			case rec.T == "compact":
				add("summary", rec.Summary)
			case rec.T == "msg" && rec.Msg != nil && !rec.Msg.Synthetic:
				for _, b := range rec.Msg.Blocks {
					switch b.Type {
					case llm.Text:
						if !strings.HasPrefix(b.Text, "<time>") {
							add(rec.Msg.Role, b.Text)
						}
					case llm.ToolCall:
						add("call "+b.Name, string(b.Input))
					case llm.ToolResult:
						add("result "+b.Name, b.Text)
					}
				}
			}
		}
		if err != nil {
			return out
		}
	}
}

// shifted maps a match found in text to its position after each newline was
// replaced by a longer marker, so the window still centres on the match.
func shifted(loc []int, text string) []int {
	shift := strings.Count(text[:loc[0]], "\n") * (len(" ⏎ ") - 1)
	return []int{loc[0] + shift, loc[1] + shift + strings.Count(text[loc[0]:loc[1]], "\n")*(len(" ⏎ ")-1)}
}

// window shortens a long line to the text around a match.
func window(line []byte, loc []int) string {
	const span = 300
	if len(line) <= 2*span {
		return string(bytes.ToValidUTF8(line, nil))
	}
	start, end := 0, 2*span
	if loc != nil {
		start, end = max(loc[0]-span, 0), min(loc[1]+span, len(line))
	}
	text := string(bytes.ToValidUTF8(line[start:end], nil))
	if start > 0 {
		text = "…" + text
	}
	if end < len(line) {
		text += "…"
	}
	return text
}
