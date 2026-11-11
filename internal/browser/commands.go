package browser

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// command is one invocation in flight.
type command struct {
	m   *Manager
	ctx context.Context
	cwd string
	inv *invocation
	cfg config
	s   *session // nil for commands that do not need a browser
	out *response
}

// commands is the CLI surface, in help order.
var commands []commandSpec

func init() {
	commands = []commandSpec{
		{name: "open", group: "Core", usage: "[url] [--headed|--headless] [--isolated] [--profile <path>]", help: "start the session's browser (optionally navigating) and show the page", maxArgs: 1, needsBrowser: true, run: cmdOpen},
		{name: "close", group: "Core", help: "close the session's browser; a persistent profile keeps its logins", run: cmdClose},
		{name: "goto", group: "Core", usage: "<url>", help: "navigate the current tab", minArgs: 1, maxArgs: 1, needsBrowser: true, run: cmdGoto},
		{name: "back", aliases: []string{"go-back"}, group: "Core", help: "go back in history", needsBrowser: true, run: cmdHistory},
		{name: "forward", aliases: []string{"go-forward"}, group: "Core", help: "go forward in history", needsBrowser: true, run: cmdHistory},
		{name: "reload", group: "Core", help: "reload the page", needsBrowser: true, run: cmdHistory},
		{name: "snapshot", group: "Core", usage: "[ref] [--depth N] [--filename f]", help: "accessibility snapshot of the page or of one element's subtree", maxArgs: 1, flags: []string{"depth", "filename"}, needsBrowser: true, run: cmdSnapshot},
		{name: "find", group: "Core", usage: "<text> [--regex]", help: "search the snapshot; prints matching lines with context and refs", minArgs: 1, maxArgs: 1, flags: []string{"regex"}, needsBrowser: true, run: cmdFind},
		{name: "click", group: "Core", usage: "<ref> [--button right|middle] [--modifiers Shift,Control]", help: "click an element", minArgs: 1, maxArgs: 1, flags: []string{"button", "modifiers"}, needsBrowser: true, run: cmdClick},
		{name: "dblclick", group: "Core", usage: "<ref> [--button right|middle] [--modifiers Shift,Control]", help: "double-click an element", minArgs: 1, maxArgs: 1, flags: []string{"button", "modifiers"}, needsBrowser: true, run: cmdClick},
		{name: "hover", group: "Core", usage: "<ref>", help: "move the mouse over an element", minArgs: 1, maxArgs: 1, needsBrowser: true, run: cmdHover},
		{name: "fill", group: "Core", usage: "<ref> <text> [--submit] | <ref> --secret NAME | <ref> --totp NAME", help: "replace the content of a text field (also date, time, color, range inputs)", minArgs: 1, maxArgs: 2, flags: []string{"submit", "secret", "totp"}, needsBrowser: true, run: cmdFill},
		{name: "type", group: "Core", usage: "<text> [--submit] | --secret NAME | --totp NAME", help: "type into the focused element, key by key", maxArgs: 1, flags: []string{"submit", "secret", "totp"}, needsBrowser: true, run: cmdFill},
		{name: "press", group: "Core", usage: "<key>", help: "press a key or combination: Enter, Escape, ArrowDown, Control+A, Shift+Tab", minArgs: 1, maxArgs: 1, needsBrowser: true, run: cmdPress},
		{name: "select", group: "Core", usage: "<ref> <value...>", help: "select options of a <select> by value or label", minArgs: 2, maxArgs: -1, needsBrowser: true, run: cmdSelect},
		{name: "check", group: "Core", usage: "<ref>", help: "check a checkbox or radio button", minArgs: 1, maxArgs: 1, needsBrowser: true, run: cmdCheck},
		{name: "uncheck", group: "Core", usage: "<ref>", help: "uncheck a checkbox", minArgs: 1, maxArgs: 1, needsBrowser: true, run: cmdCheck},
		{name: "upload", group: "Core", usage: "[ref] <file...>", help: "set the files of a file input, or answer an open file chooser when no ref is given", minArgs: 1, maxArgs: -1, needsBrowser: true, run: cmdUpload},
		{name: "drag", group: "Core", usage: "<from-ref> <to-ref>", help: "drag one element onto another", minArgs: 2, maxArgs: 2, needsBrowser: true, run: cmdDrag},
		{name: "eval", group: "Core", usage: "<js> [ref]", help: "evaluate an expression or function in the page; with a ref the element is the argument", minArgs: 1, maxArgs: 2, needsBrowser: true, run: cmdEval},
		{name: "wait", group: "Core", usage: "<ms> | --text T | --gone T | --url glob | --load", help: "wait for time, text to appear or disappear, a URL, or the load event", maxArgs: 1, flags: []string{"text", "gone", "url", "load"}, needsBrowser: true, run: cmdWait},
		{name: "dialog-accept", group: "Core", usage: "[text]", help: "accept the open alert/confirm/prompt, optionally entering prompt text", maxArgs: 1, needsBrowser: true, run: cmdDialog},
		{name: "dialog-dismiss", group: "Core", help: "dismiss the open dialog", needsBrowser: true, run: cmdDialog},
		{name: "screenshot", group: "Core", usage: "[ref] [--full] [--filename f]", help: "PNG of the viewport, the full page or one element", maxArgs: 1, flags: []string{"full", "filename"}, needsBrowser: true, run: cmdScreenshot},

		{name: "tab-list", group: "Tabs", help: "list open tabs", needsBrowser: true, run: cmdTabList},
		{name: "tab-new", group: "Tabs", usage: "[url]", help: "open a tab and switch to it", maxArgs: 1, needsBrowser: true, run: cmdTabNew},
		{name: "tab-select", group: "Tabs", usage: "<index>", help: "switch to a tab", minArgs: 1, maxArgs: 1, needsBrowser: true, run: cmdTabSelect},
		{name: "tab-close", group: "Tabs", usage: "[index]", help: "close a tab (default: the current one)", maxArgs: 1, needsBrowser: true, run: cmdTabClose},

		{name: "state-save", group: "Storage", usage: "[file]", help: "save cookies and localStorage to a JSON file", maxArgs: 1, needsBrowser: true, run: cmdStateSave},
		{name: "state-load", group: "Storage", usage: "<file>", help: "restore cookies and localStorage from a state file", minArgs: 1, maxArgs: 1, needsBrowser: true, run: cmdStateLoad},
		{name: "cookie-list", group: "Storage", usage: "[--domain d]", help: "list cookies", flags: []string{"domain"}, needsBrowser: true, run: cmdCookieList},
		{name: "cookie-clear", group: "Storage", help: "delete all cookies", needsBrowser: true, run: cmdCookieClear},

		{name: "console", group: "DevTools", usage: "[--level error|warning|info|debug]", help: "console messages since the last navigation, at the given level or more severe", maxArgs: 1, flags: []string{"level"}, needsBrowser: true, run: cmdConsole},
		{name: "network", group: "DevTools", help: "network requests since the last navigation", needsBrowser: true, run: cmdNetwork},

		{name: "handoff", group: "Human", usage: "<reason>", help: "ask the user to take over (CAPTCHA, SMS code, passkey, unusual login check); blocks until they are done", minArgs: 1, maxArgs: -1, needsBrowser: true, run: cmdHandoff},

		{name: "status", group: "Setup", help: "list running sessions", run: cmdStatus},
		{name: "install", group: "Setup", usage: "[--force]", help: "download or update Chrome for Testing", flags: []string{"force"}, run: cmdInstall},
		{name: "doctor", group: "Setup", help: "show which browser will be used and check its system dependencies", run: cmdDoctor},
		{name: "config", group: "Setup", help: "print the effective configuration and where to edit it", run: cmdConfig},
		{name: "help", group: "Setup", usage: "[command]", help: "show this reference", maxArgs: 1, run: cmdHelp},
	}
}

// show renders a path for the reply: relative to the caller's working
// directory when the file is inside it, which is shorter and is what the
// caller would type; absolute otherwise.
func (c *command) show(path string) string {
	if rel, err := filepath.Rel(c.cwd, path); err == nil && c.cwd != "" && filepath.IsLocal(rel) {
		return filepath.ToSlash(rel)
	}
	return path
}

// resolvePath resolves a path argument against the caller's working
// directory.
func (c *command) resolvePath(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(c.cwd, p)
}

// timeout returns the action timeout: --timeout if given, else the config's.
func (c *command) timeout() (time.Duration, error) {
	return c.inv.timeout(c.cfg.actionTimeout())
}

// page returns the current tab, refusing to work on a page whose scripts are
// blocked by a dialog.
func (c *command) page() (*tab, error) {
	t, err := c.s.currentTab(c.ctx)
	if err != nil {
		return nil, err
	}
	if d := t.pendingDialog(); d != nil {
		return nil, fmt.Errorf("a %q dialog is open with message %q; handle it with `dialog-accept` or `dialog-dismiss` first", d.kind, d.message)
	}
	return t, nil
}

// do runs an element action on the current page with the action timeout,
// then lets the page settle and includes its snapshot in the reply.
func (c *command) do(fn func(ctx context.Context, t *tab, timeout time.Duration) error) error {
	return c.withPage(c.cfg.actionTimeout(), fn)
}

func (c *command) withPage(def time.Duration, fn func(ctx context.Context, t *tab, timeout time.Duration) error) error {
	t, err := c.page()
	if err != nil {
		return err
	}
	timeout, err := c.inv.timeout(def)
	if err != nil {
		return err
	}
	return c.act(t, func(ctx context.Context) error { return fn(ctx, t, timeout) })
}

// act runs a page-changing action, waits for the page to settle and asks for
// a snapshot in the reply. An action cut short by a dialog it opened, or by
// closing its own tab, counts as done: the reply shows the new state.
func (c *command) act(t *tab, fn func(ctx context.Context) error) error {
	since := t.beginAction()
	ctx, cancel := t.guard(c.ctx)
	err := fn(ctx)
	cancel()
	if err != nil && c.ctx.Err() == nil && (t.pendingDialog() != nil || t.isClosed()) {
		err = nil
	}
	if err != nil {
		return err
	}
	t.settle(c.ctx, since)
	c.out.snapshot = true
	return nil
}

func cmdOpen(c *command) error {
	c.out.say("Browser session %q is open (%s, profile: %s).", c.s.name, c.s.describeMode(), c.show(c.s.profileLabel()))
	if url := c.inv.arg(0); url != "" {
		return c.navigate(url)
	}
	_, err := c.page()
	c.out.snapshot = true
	return err
}

func cmdClose(c *command) error {
	s := c.m.lookup(c.inv.session)
	if s == nil {
		c.out.say("Browser session %q is not open.", c.inv.session)
		return nil
	}
	s.cmdMu.Lock()
	s.close()
	s.cmdMu.Unlock()
	c.out.say("Closed browser session %q.", s.name)
	return nil
}

func cmdGoto(c *command) error { return c.navigate(c.inv.arg(0)) }

// navigate loads a URL in the current tab and reports the resulting page.
func (c *command) navigate(raw string) error {
	url := normalizeURL(raw)
	if !c.s.filter.allows(url) {
		return fmt.Errorf("%s is blocked by the network.allowedOrigins/blockedOrigins configuration", url)
	}
	err := c.withPage(c.cfg.navigationTimeout(), func(ctx context.Context, t *tab, timeout time.Duration) error {
		return t.navigate(ctx, url, timeout)
	})
	if err != nil {
		return err
	}
	c.out.say("Navigated to %s", url)
	return nil
}

// cmdHistory implements back, forward and reload.
func cmdHistory(c *command) error {
	err := c.withPage(c.cfg.navigationTimeout(), func(ctx context.Context, t *tab, timeout time.Duration) error {
		switch c.inv.cmd {
		case "back":
			return t.history(ctx, -1, timeout)
		case "forward":
			return t.history(ctx, 1, timeout)
		}
		return t.reload(ctx, timeout)
	})
	if err != nil {
		return err
	}
	c.out.say("%s", map[string]string{"back": "Went back", "forward": "Went forward", "reload": "Reloaded the page"}[c.inv.cmd])
	return nil
}

func cmdSnapshot(c *command) error {
	if _, err := c.page(); err != nil {
		return err
	}
	depth, err := c.inv.intFlag("depth", 0)
	if err != nil {
		return err
	}
	if root := c.inv.arg(0); root != "" && !refPattern.MatchString(root) {
		return fmt.Errorf("%q is not a ref; pass a ref from the snapshot such as e12", root)
	}
	c.out.snapshot, c.out.snapRoot, c.out.snapDepth = true, c.inv.arg(0), depth
	if name := c.inv.flags["filename"]; name != "" {
		c.out.snapFile = c.resolvePath(name)
	}
	return nil
}

func cmdDialog(c *command) error {
	t, err := c.s.currentTab(c.ctx)
	if err != nil {
		return err
	}
	d := t.pendingDialog()
	if d == nil {
		return errors.New("no dialog is open")
	}
	accept := c.inv.cmd == "dialog-accept"
	params := obj{"accept": accept}
	if accept && d.kind == "prompt" {
		params["promptText"] = d.defaultPrompt
		if len(c.inv.args) > 0 {
			params["promptText"] = c.inv.arg(0)
		}
	}
	since := t.beginAction()
	if err := d.sess.call(c.ctx, "Page.handleJavaScriptDialog", params, nil); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	c.s.wait(ctx, func() bool { return t.dialog != d })
	t.settle(c.ctx, since)
	verb := "Dismissed"
	if accept {
		verb = "Accepted"
	}
	c.out.say("%s the %q dialog", verb, d.kind)
	c.out.snapshot = true
	return nil
}

func cmdStatus(c *command) error {
	sessions := c.m.Status()
	if len(sessions) == 0 {
		c.out.say("No browser sessions are running. `open [url]` starts one.")
		return nil
	}
	for _, st := range sessions {
		mode := "headed"
		if st.Headless {
			mode = "headless"
		}
		line := fmt.Sprintf("- %s: %s, %d tab(s), current: [%s](%s)", st.Name, mode, st.Tabs, st.Title, st.URL)
		if st.Handoff != nil {
			line += fmt.Sprintf(" - waiting for the user: %s", st.Handoff.Reason)
		}
		c.out.say("%s", line)
	}
	return nil
}
