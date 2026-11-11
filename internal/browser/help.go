package browser

import (
	"fmt"
	"strings"
)

const helpIntro = `artus browser - drive a Chromium browser from the command line

Usage: artus browser <command> [arguments] [flags]

Every command prints what happened, the page URL and title, and (after
actions) an accessibility snapshot of the page:

  - heading "Sign in" [level=1] [ref=e3]
  - textbox "Email" [ref=e5]: user@example.com
  - iframe [ref=e11]:
    - button "Pay" [ref=f1e2]

Address elements by their ref (e5, f1e2). Refs come from the latest snapshot;
when the page changes, use the refs of the new snapshot. A CSS selector for
the main document works wherever a ref does. Actions wait for the element to
be visible, stable and enabled, scroll it into view and use real input events.
Large snapshots are written to a file; use "find" to search them.
`

const helpOutro = `Global flags (anywhere on the command line):
  -s=<name>, --session <name>  browser session to use (default "default");
                               each session is its own browser and profile
  --headed, --headless         show or hide the browser window when it starts
  --isolated                   start with a throwaway profile
  --profile <path>             start with the profile in this directory
  --timeout <ms>               override the timeout of this command
  --                           treat everything after it as arguments

Secrets: "fill <ref> --secret NAME" and "type --secret NAME" enter a stored
secret, "--totp NAME" the current one-time code, without revealing them.

When a site needs a human (CAPTCHA, SMS code, passkey, "is this you?"), do not
try to work around it: run "handoff <reason>" and continue when it returns.

Dialogs (alert/confirm/prompt) block the page until "dialog-accept" or
"dialog-dismiss". Logins persist per session in its profile; "state-save" and
"state-load" copy cookies and localStorage between sessions.

Actions scroll to their target themselves; to scroll for reading use "press
PageDown" or "press End". Anything else a page offers is within reach of
"eval", for example localStorage or the size of the window.
`

// Help returns the command reference, as printed by `artus browser help`.
func Help() string {
	var b strings.Builder
	b.WriteString(helpIntro)
	group := ""
	for _, spec := range commands {
		if spec.group != group {
			group = spec.group
			b.WriteString("\n" + group + ":\n")
		}
		names := append([]string{spec.name}, spec.aliases...)
		line := strings.TrimSpace(strings.Join(names, "|") + " " + spec.usage)
		if spec.help == "" {
			fmt.Fprintf(&b, "  %s\n", line)
		} else if len(line) > 34 {
			fmt.Fprintf(&b, "  %s\n  %-34s  %s\n", line, "", spec.help)
		} else {
			fmt.Fprintf(&b, "  %-34s  %s\n", line, spec.help)
		}
	}
	b.WriteString("\n" + helpOutro)
	return b.String()
}

func cmdHelp(c *command) error {
	if name := c.inv.arg(0); name != "" {
		spec := findCommand(name)
		if spec == nil {
			return fmt.Errorf("unknown command %q", name)
		}
		c.out.say("artus browser %s", strings.TrimSpace(spec.name+" "+spec.usage))
		if spec.help != "" {
			c.out.say("  %s", spec.help)
		}
		if len(spec.aliases) > 0 {
			c.out.say("  alias: %s", strings.Join(spec.aliases, ", "))
		}
		return nil
	}
	c.out.raw = Help()
	return nil
}
