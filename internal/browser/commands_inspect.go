package browser

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func cmdConsole(c *command) error {
	t, err := c.s.currentTab(c.ctx)
	if err != nil {
		return err
	}
	level := "info"
	if v, ok := c.inv.flags["level"]; ok {
		level = v
	} else if len(c.inv.args) > 0 {
		level = c.inv.arg(0)
	}
	threshold := slices.Index(consoleLevels, level)
	if threshold < 0 {
		return fmt.Errorf("the level must be one of %s, not %q", strings.Join(consoleLevels, ", "), level)
	}
	c.s.mu.Lock()
	var lines []string
	for _, e := range t.console {
		if slices.Index(consoleLevels, e.level) <= threshold {
			lines = append(lines, e.String())
		}
	}
	t.consoleReported = len(t.console)
	c.s.mu.Unlock()
	if len(lines) == 0 {
		c.out.say("No console messages at level %q or above since the last navigation.", level)
	}
	c.out.lines = append(c.out.lines, lines...)
	return nil
}

func cmdNetwork(c *command) error {
	t, err := c.s.currentTab(c.ctx)
	if err != nil {
		return err
	}
	c.s.mu.Lock()
	for _, r := range t.requests {
		c.out.lines = append(c.out.lines, r.String())
	}
	c.s.mu.Unlock()
	if len(c.out.lines) == 0 {
		c.out.say("No requests since the last navigation.")
	}
	return nil
}

// findContext is how many lines of context find shows around a match.
const findContext = 3

func cmdFind(c *command) error {
	t, err := c.page()
	if err != nil {
		return err
	}
	query := c.inv.arg(0)
	match := func(line string) bool { return strings.Contains(strings.ToLower(line), strings.ToLower(query)) }
	shown := strconv.Quote(query)
	if c.inv.has("regex") {
		// /pattern/flags selects flags, as in JavaScript; only i is useful here.
		pattern := query
		if m := regexp.MustCompile(`^/(.*)/([a-z]*)$`).FindStringSubmatch(query); m != nil {
			pattern = m[1]
			if strings.Contains(m[2], "i") {
				pattern = "(?i)" + pattern
			}
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("invalid regular expression: %w", err)
		}
		match, shown = re.MatchString, query
	}
	ctx, cancel := t.guard(c.ctx)
	defer cancel()
	snapshot, err := t.snapshot(ctx, "", 0)
	if err != nil {
		return err
	}
	lines := strings.Split(c.m.redact(snapshot), "\n")
	indent := func(i int) int { return len(lines[i]) - len(strings.TrimLeft(lines[i], " ")) }
	show := map[int]bool{}
	matches := 0
	for i, line := range lines {
		if !match(line) {
			continue
		}
		matches++
		for j := max(0, i-findContext); j <= min(len(lines)-1, i+findContext); j++ {
			show[j] = true
		}
		// Ancestors give the match its place in the page.
		for j, level := i-1, indent(i); j >= 0 && level > 0; j-- {
			if indent(j) < level {
				show[j], level = true, indent(j)
			}
		}
	}
	if matches == 0 {
		c.out.say("No matches for %s in the page snapshot.", shown)
		return nil
	}
	c.out.say("Found %d matching line(s) for %s:", matches, shown)
	last := -1
	for i := range lines {
		if !show[i] {
			continue
		}
		if last >= 0 && i > last+1 {
			c.out.say("%s...", strings.Repeat(" ", indent(i)))
		}
		c.out.say("%s", lines[i])
		last = i
	}
	return nil
}
