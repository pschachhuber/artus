package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func cmdClick(c *command) error {
	opts := clickOptions{button: "left", count: 1}
	if b, ok := c.inv.flags["button"]; ok {
		if mouseButtons[b] == 0 {
			return fmt.Errorf("--button must be left, right or middle, not %q", b)
		}
		opts.button = b
	}
	var err error
	if opts.modifiers, err = parseModifiers(c.inv.flags["modifiers"]); err != nil {
		return err
	}
	verb := "Clicked"
	if c.inv.cmd == "dblclick" {
		opts.count, verb = 2, "Double-clicked"
	}
	label := c.inv.arg(0)
	err = c.do(func(ctx context.Context, t *tab, timeout time.Duration) error {
		l, err := t.click(ctx, c.inv.arg(0), timeout, opts)
		if l != "" {
			label = l
		}
		return err
	})
	if err != nil {
		return err
	}
	c.out.say("%s %s", verb, label)
	return nil
}

func cmdHover(c *command) error {
	var label string
	err := c.do(func(ctx context.Context, t *tab, timeout time.Duration) (err error) {
		label, err = t.pointer(ctx, c.inv.arg(0), timeout, false, func(ctx context.Context, _ *element, pt point) error {
			return t.mouseMove(ctx, pt)
		})
		return err
	})
	if err != nil {
		return err
	}
	c.out.say("Hovering over %s", label)
	return nil
}

// textArg returns the text a fill or type command should enter and how to
// describe it in the reply. Secrets are resolved here and never echoed.
func (c *command) textArg(index int) (text, shown string, err error) {
	secret, isSecret := c.inv.flags["secret"]
	totp, isTOTP := c.inv.flags["totp"]
	given := len(c.inv.args) > index
	switch {
	case isSecret && isTOTP, (isSecret || isTOTP) && given:
		return "", "", errors.New("pass exactly one of: the text, --secret NAME, or --totp NAME")
	case isSecret:
		text, err = c.m.secret(secret, false)
		return text, "secret " + secret, err
	case isTOTP:
		text, err = c.m.secret(totp, true)
		return text, "the current one-time code of " + totp, err
	case !given:
		return "", "", fmt.Errorf("usage: artus browser %s %s", c.inv.cmd, c.inv.spec.usage)
	}
	return c.inv.arg(index), strconv.Quote(c.inv.arg(index)), nil
}

// cmdFill implements fill and type; type has no target and sends keystrokes
// to whatever is focused.
func cmdFill(c *command) error {
	typing := c.inv.cmd == "type"
	index := 1
	if typing {
		index = 0
	}
	text, shown, err := c.textArg(index)
	if err != nil {
		return err
	}
	var label string
	err = c.do(func(ctx context.Context, t *tab, timeout time.Duration) (err error) {
		if typing {
			err = t.typeText(ctx, text)
		} else {
			label, err = t.fill(ctx, c.inv.arg(0), text, timeout)
		}
		if err == nil && c.inv.has("submit") {
			err = t.pressKey(ctx, "Enter")
		}
		return err
	})
	if err != nil {
		return err
	}
	submitted := ""
	if c.inv.has("submit") {
		submitted = " and pressed Enter"
	}
	if typing {
		c.out.say("Typed %s%s", shown, submitted)
	} else {
		c.out.say("Filled %s with %s%s", label, shown, submitted)
	}
	return nil
}

func cmdPress(c *command) error {
	if _, err := parseKeyCombo(c.inv.arg(0)); err != nil {
		return err
	}
	err := c.do(func(ctx context.Context, t *tab, _ time.Duration) error { return t.pressKey(ctx, c.inv.arg(0)) })
	if err != nil {
		return err
	}
	c.out.say("Pressed %s", c.inv.arg(0))
	return nil
}

func cmdSelect(c *command) error {
	var label string
	var selected []string
	err := c.do(func(ctx context.Context, t *tab, timeout time.Duration) (err error) {
		label, selected, err = t.selectOptions(ctx, c.inv.arg(0), c.inv.args[1:], timeout)
		return err
	})
	if err != nil {
		return err
	}
	for i, v := range selected {
		selected[i] = strconv.Quote(v)
	}
	c.out.say("Selected %s in %s", strings.Join(selected, ", "), label)
	return nil
}

// cmdCheck implements check and uncheck.
func cmdCheck(c *command) error {
	want, state := c.inv.cmd == "check", c.inv.cmd+"ed"
	var label string
	var changed bool
	err := c.do(func(ctx context.Context, t *tab, timeout time.Duration) (err error) {
		label, changed, err = t.setChecked(ctx, c.inv.arg(0), want, timeout)
		return err
	})
	if err != nil {
		return err
	}
	if changed {
		c.out.say("%s is now %s", label, state)
	} else {
		c.out.say("%s was already %s", label, state)
	}
	return nil
}

func cmdUpload(c *command) error {
	// The first argument is the target unless it names an existing file.
	target, names := "", c.inv.args
	if _, err := os.Stat(c.resolvePath(names[0])); err != nil && len(names) > 1 {
		target, names = names[0], names[1:]
	}
	files := make([]string, len(names))
	for i, name := range names {
		files[i] = c.resolvePath(name)
		if info, err := os.Stat(files[i]); err != nil {
			return fmt.Errorf("cannot upload %s: %w", name, err)
		} else if info.IsDir() {
			return fmt.Errorf("cannot upload %s: it is a directory", name)
		}
	}
	label := "the open file chooser"
	err := c.do(func(ctx context.Context, t *tab, timeout time.Duration) (err error) {
		if target == "" {
			return t.acceptChooser(ctx, files)
		}
		label, err = t.upload(ctx, target, files, timeout)
		return err
	})
	if err != nil {
		return err
	}
	c.out.say("Uploaded %s to %s", strings.Join(names, ", "), label)
	return nil
}

func cmdDrag(c *command) error {
	var from, to string
	err := c.do(func(ctx context.Context, t *tab, timeout time.Duration) (err error) {
		from, to, err = t.drag(ctx, c.inv.arg(0), c.inv.arg(1), timeout)
		return err
	})
	if err != nil {
		return err
	}
	c.out.say("Dragged %s onto %s", from, to)
	return nil
}
