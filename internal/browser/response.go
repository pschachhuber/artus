package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// snapshotHeadLines is how much of an oversized snapshot is shown inline.
	snapshotHeadLines = 40
	// snapshotKeep is how many snapshot files are kept per session.
	snapshotKeep = 50
)

// response collects what a command reports. The reply is assembled once the
// command is done, when the page has settled.
type response struct {
	lines []string // the command's own result
	files []string
	raw   string // a complete reply, bypassing all formatting

	page      bool   // report the page's URL and title
	tabs      bool   // list the open tabs even when there is only one
	snapshot  bool   // report the page and include its snapshot
	snapRoot  string // limit the snapshot to this ref's subtree
	snapDepth int
	snapFile  string // write the snapshot here and only link to it
}

func (r *response) say(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

// result renders the reply:
//
//	### Result
//	Clicked button "Sign in" (e5)
//	### Page
//	- Page URL: https://example.com/account
//	- Page Title: Account
//	### Snapshot
//	```yaml
//	- heading "Account" [level=1] [ref=e2]
//	```
func (r *response) result(c *command) (Result, error) {
	if r.raw != "" {
		return Result{Text: r.raw}, nil
	}
	var b strings.Builder
	section := func(title string, lines ...string) {
		if len(lines) > 0 {
			b.WriteString("### " + title + "\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	section("Result", r.lines...)
	if s := c.s; s != nil && !s.dead() && (r.page || r.tabs || r.snapshot) {
		r.pageState(c, section)
	}
	return Result{Text: strings.TrimRight(b.String(), "\n"), Files: r.files}, nil
}

// pageState appends the tab list, page header, pending dialogs, events and
// the snapshot.
func (r *response) pageState(c *command, section func(title string, lines ...string)) {
	s := c.s
	ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
	defer cancel()
	s.awaitOpened(ctx)

	tabs, current := s.tabList()
	if len(tabs) != 1 || r.tabs {
		lines := make([]string, len(tabs))
		for i, t := range tabs {
			url, title := t.urlAndTitle(ctx)
			marker := ""
			if i == current {
				marker = " (current)"
			}
			lines[i] = fmt.Sprintf("- %d:%s [%s](%s)", i, marker, title, url)
		}
		if len(tabs) == 0 {
			lines = []string{"No open tabs. `goto <url>` or `tab-new` opens one."}
		}
		section("Open tabs", lines...)
	}
	if current < 0 {
		return
	}
	t := tabs[current]
	url, title := t.urlAndTitle(ctx)

	s.mu.Lock()
	d, chooser, crashed, status := t.dialog, t.chooser, t.crashed, t.docStatus
	errorCount, warnings := t.consoleSince()
	var events []string
	for _, n := range s.notes {
		if n.path != "" {
			n.text += " " + strconv.Quote(c.show(n.path))
		}
		events = append(events, n.text)
	}
	for _, name := range s.downloading() {
		events = append(events, fmt.Sprintf("Downloading file %s ... (a later command reports when it is done)", name))
	}
	for _, opened := range s.opened {
		for i, other := range tabs {
			if other == opened {
				events = append(events, fmt.Sprintf("A new tab opened (tab %d); `tab-select %d` switches to it", i, i))
			}
		}
	}
	r.files = append(r.files, s.files...)
	s.notes, s.files, s.opened = nil, nil, nil
	s.mu.Unlock()

	header := []string{"- Page URL: " + url}
	if title != "" {
		header = append(header, "- Page Title: "+title)
	}
	if crashed {
		header = append(header, "- Page status: crashed (`reload` or `goto` recovers it)")
	}
	if status >= 400 {
		header = append(header, fmt.Sprintf("- HTTP status: %d", status))
	}
	if errorCount+warnings > 0 {
		header = append(header, fmt.Sprintf("- Console: %d new errors, %d new warnings (`console` shows them)", errorCount, warnings))
	}
	section("Page", header...)

	var modal []string
	if d != nil {
		modal = append(modal, fmt.Sprintf("- [%q dialog with message %q]: handle it with `dialog-accept` or `dialog-dismiss`; the page is blocked until then", d.kind, d.message))
	}
	if chooser != nil {
		modal = append(modal, "- [File chooser]: provide files with `upload <file...>`")
	}
	section("Modal state", modal...)
	for i, e := range events {
		events[i] = "- " + e
	}
	section("Events", events...)

	if r.snapshot && d == nil && !crashed {
		section("Snapshot", r.snapshotLines(ctx, c, t)...)
	}
}

// snapshotLines captures the snapshot, stores it under snapshots/ and
// returns it inline when small enough, or as a file link with its first
// lines otherwise.
func (r *response) snapshotLines(ctx context.Context, c *command, t *tab) []string {
	s := c.s
	ctx, cancel := t.guard(ctx)
	defer cancel()
	yaml, err := t.snapshot(ctx, r.snapRoot, r.snapDepth)
	if err != nil {
		if t.pendingDialog() != nil {
			return nil
		}
		return []string{fmt.Sprintf("The snapshot is not available (%v). Run `snapshot` to retry.", err)}
	}
	yaml = c.m.redact(yaml)
	s.mu.Lock()
	s.snapSeq++
	seq, limit := s.snapSeq, s.cfg.Snapshot.InlineMaxChars
	s.mu.Unlock()
	dir := filepath.Join(c.m.opts.Dir, "snapshots")
	file := filepath.Join(dir, fmt.Sprintf("%s-%d.yml", s.name, seq))
	os.Remove(filepath.Join(dir, fmt.Sprintf("%s-%d.yml", s.name, seq-snapshotKeep)))
	if r.snapFile != "" {
		file = r.snapFile
	}
	err = os.MkdirAll(filepath.Dir(file), 0o755)
	if err == nil {
		err = os.WriteFile(file, []byte(yaml+"\n"), 0o644)
	}
	switch {
	case r.snapFile != "" && err != nil:
		return []string{fmt.Sprintf("The snapshot could not be written to %s: %v", c.show(file), err)}
	case r.snapFile != "":
		r.files = append(r.files, file)
		return []string{fmt.Sprintf("- [Snapshot](%s)", c.show(file))}
	case yaml == "":
		return []string{"The page has no accessible content yet."}
	case len(yaml) <= limit || err != nil:
		return []string{"```yaml", yaml, "```"}
	}
	lines := strings.Split(yaml, "\n")
	head := lines[:min(len(lines), snapshotHeadLines)]
	return []string{
		fmt.Sprintf("- [Snapshot](%s)", c.show(file)),
		fmt.Sprintf("The snapshot has %d lines, too many to show. Search it with `find <text>`, expand one element with `snapshot <ref>`, or read the file. It starts with:", len(lines)),
		"```yaml", strings.Join(head, "\n"), "```",
	}
}

// awaitOpened gives tabs that an action just opened a moment to start and
// finish loading, so that the reply can name them.
func (s *session) awaitOpened(ctx context.Context) {
	all := func(ok func(f *frame) bool) func() bool {
		return func() bool {
			for _, t := range s.opened {
				if f := t.main(); f == nil || !ok(f) {
					return false
				}
			}
			return true
		}
	}
	started, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	s.wait(started, all(func(f *frame) bool { return f.loading || (f.url != "" && f.url != "about:blank") }))
	loaded, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	s.wait(loaded, all(func(f *frame) bool { return !f.loading }))
}
