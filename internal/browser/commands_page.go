package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// evalFunction wraps user code for an element: an expression, or a function
// that receives the element.
func evalFunction(source string) string {
	return "async function() { const value = (\n" + source + "\n); return typeof value === 'function' ? value(this) : value; }"
}

func cmdEval(c *command) error {
	var res evalResult
	err := c.do(func(ctx context.Context, t *tab, timeout time.Duration) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		err := t.evaluate(ctx, c.inv.arg(0), c.inv.arg(1), timeout, &res)
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("the code did not finish within %s; pass --timeout <ms> to allow more", timeout)
		}
		return err
	})
	if err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return errors.New(res.ExceptionDetails.message())
	}
	value := "undefined"
	if len(res.Result.Value) > 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, res.Result.Value, "", "  ") == nil {
			value = pretty.String()
		}
	} else if res.Result.Type != "undefined" && res.Result.Type != "" {
		value = "[" + res.Result.Type + "]"
	}
	c.out.say("%s", value)
	c.out.snapshot = false
	return nil
}

// evaluate runs user code in the page's own JavaScript world: against the
// target element when one is given, else in the main frame.
func (t *tab) evaluate(ctx context.Context, source, target string, timeout time.Duration, res *evalResult) error {
	if target != "" {
		return t.retry(ctx, target, timeout, func(ctx context.Context, el *element, try int) (string, error) {
			var node struct {
				Object struct {
					ObjectID string `json:"objectId"`
				} `json:"object"`
			}
			if err := el.frame.sess.call(ctx, "DOM.resolveNode", obj{"backendNodeId": el.backend}, &node); err != nil {
				return "", err
			}
			return "", el.frame.sess.call(ctx, "Runtime.callFunctionOn", obj{
				"objectId": node.Object.ObjectID, "functionDeclaration": evalFunction(source),
				"awaitPromise": true, "returnByValue": true, "userGesture": true,
			}, res)
		})
	}
	params := obj{
		"expression":   "(async () => { const value = (\n" + source + "\n); return typeof value === 'function' ? value() : value; })()",
		"awaitPromise": true, "returnByValue": true, "userGesture": true,
	}
	if err := t.sess.call(ctx, "Runtime.evaluate", params, res); err != nil {
		return err
	}
	if d := res.ExceptionDetails; d != nil && strings.HasPrefix(d.message(), "SyntaxError") {
		// Not an expression: run it as a script and use its completion value.
		*res = evalResult{}
		params["expression"], params["replMode"] = source, true
		return t.sess.call(ctx, "Runtime.evaluate", params, res)
	}
	return nil
}

const maxWait = 30 * time.Second

func cmdWait(c *command) error {
	t, err := c.page()
	if err != nil {
		return err
	}
	inv := c.inv
	timeout, err := c.timeout()
	if err != nil {
		return err
	}
	ctx, cancel := t.guard(c.ctx)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, timeout)
	defer stop()
	// waited reports a timeout as an error; a wait cut short by a dialog or
	// by the tab closing just ends, and the reply shows why.
	waited := func(what, success string) error {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		case ctx.Err() != nil:
			c.out.say("Stopped waiting for %s: a dialog opened or the tab closed", what)
			return c.ctx.Err()
		}
		c.out.say("%s", success)
		return nil
	}
	switch {
	case inv.has("text") || inv.has("gone"):
		text, want, verb := inv.flags["text"], true, "appear"
		if inv.has("gone") {
			text, want, verb = inv.flags["gone"], false, "disappear"
		}
		for ctx.Err() == nil && t.hasText(ctx, text) != want {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
			}
		}
		err = waited(fmt.Sprintf("text %q to %s", text, verb), fmt.Sprintf("Text %q %sed", text, verb))
	case inv.has("url"):
		pattern, perr := globToRegexp(inv.flags["url"])
		if perr != nil {
			return perr
		}
		t.s.wait(ctx, func() bool { f := t.main(); return f != nil && pattern.MatchString(f.url) })
		err = waited(fmt.Sprintf("the URL to match %q (it is %s)", inv.flags["url"], t.url()), "URL is "+t.url())
	case inv.has("load"):
		t.waitLoad(ctx)
		err = waited("the page to load", "Page loaded")
	default:
		ms, aerr := strconv.Atoi(inv.arg(0))
		if aerr != nil || ms < 0 {
			return fmt.Errorf("usage: artus browser wait %s", inv.spec.usage)
		}
		d := min(time.Duration(ms)*time.Millisecond, maxWait)
		select {
		case <-time.After(d):
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
		c.out.say("Waited %s", d)
	}
	if err != nil {
		return err
	}
	t.settle(c.ctx, time.Now())
	c.out.snapshot = true
	return nil
}

// hasText reports whether any frame of the tab shows text.
func (t *tab) hasText(ctx context.Context, text string) bool {
	for _, f := range t.frameRefs() {
		var res struct {
			Found bool `json:"found"`
		}
		if t.callFrame(ctx, f, "hasText", obj{"text": text}, &res) == nil && res.Found {
			return true
		}
	}
	return false
}

// globToRegexp compiles a URL glob: "*" matches within a path segment, "**"
// across segments, "?" one character. A pattern without wildcards matches as
// a substring.
func globToRegexp(glob string) (*regexp.Regexp, error) {
	if !strings.ContainsAny(glob, "*?") {
		return regexp.Compile(regexp.QuoteMeta(glob))
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch {
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case glob[i] == '*':
			b.WriteString("[^/]*")
		case glob[i] == '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// outputFile returns where a command should write an artefact: the name the
// caller gave, resolved against its directory, or a fresh name under output/.
func (c *command) outputFile(given, kind, ext string) (string, error) {
	file := filepath.Join(c.m.opts.Dir, "output", fmt.Sprintf("%s-%s-%s.%s", c.s.name, kind, time.Now().Format("20060102-150405.000"), ext))
	if given != "" {
		file = c.resolvePath(given)
	}
	return file, os.MkdirAll(filepath.Dir(file), 0o755)
}

func cmdScreenshot(c *command) error {
	t, err := c.page()
	if err != nil {
		return err
	}
	timeout, err := c.timeout()
	if err != nil {
		return err
	}
	file, err := c.outputFile(c.inv.flags["filename"], "screenshot", "png")
	if err != nil {
		return err
	}
	ctx, cancel := t.guard(c.ctx)
	defer cancel()
	params := obj{"format": "png"}
	what := "the viewport"
	switch target := c.inv.arg(0); {
	case target != "":
		err = t.retry(ctx, target, timeout, func(ctx context.Context, el *element, try int) (string, error) {
			what = el.label
			if reason, err := el.waitStates(ctx, "visible", "stable"); reason != "" || err != nil {
				return reason, err
			}
			clip, err := el.documentRect(ctx)
			params["clip"], params["captureBeyondViewport"] = clip, true
			return "", err
		})
		if err != nil {
			return err
		}
	case c.inv.has("full"):
		var m struct {
			CSSContentSize struct {
				Width  float64 `json:"width"`
				Height float64 `json:"height"`
			} `json:"cssContentSize"`
		}
		if err := t.sess.call(ctx, "Page.getLayoutMetrics", nil, &m); err != nil {
			return err
		}
		what = "the full page"
		params["clip"] = obj{"x": 0, "y": 0, "width": m.CSSContentSize.Width, "height": m.CSSContentSize.Height, "scale": 1}
		params["captureBeyondViewport"] = true
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := t.sess.call(ctx, "Page.captureScreenshot", params, &shot); err != nil {
		return err
	}
	if err := writeBase64(file, shot.Data); err != nil {
		return err
	}
	c.out.say("Screenshot of %s saved to %s", what, c.show(file))
	c.out.files = append(c.out.files, file)
	return nil
}

// documentRect returns the element's bounding box in page coordinates, as
// Page.captureScreenshot expects for a clip.
func (el *element) documentRect(ctx context.Context) (obj, error) {
	objectID, err := el.object(ctx)
	if err != nil {
		return nil, err
	}
	el.frame.sess.call(ctx, "DOM.scrollIntoViewIfNeeded", obj{"objectId": objectID}, nil)
	quads, err := contentQuads(ctx, el.frame.sess, obj{"objectId": objectID})
	if err != nil || len(quads) == 0 {
		return nil, errors.Join(errors.New("the element has no visible box to capture"), err)
	}
	base, err := el.t.rootOrigin(ctx, el.frame)
	if err != nil {
		return nil, err
	}
	var m struct {
		CSSVisualViewport struct {
			PageX float64 `json:"pageX"`
			PageY float64 `json:"pageY"`
		} `json:"cssVisualViewport"`
	}
	if err := el.t.sess.call(ctx, "Page.getLayoutMetrics", nil, &m); err != nil {
		return nil, err
	}
	minX, minY, maxX, maxY := quads[0][0].x, quads[0][0].y, quads[0][0].x, quads[0][0].y
	for _, q := range quads {
		for _, p := range q {
			minX, minY, maxX, maxY = min(minX, p.x), min(minY, p.y), max(maxX, p.x), max(maxY, p.y)
		}
	}
	return obj{
		"x": minX + base.x + m.CSSVisualViewport.PageX, "y": minY + base.y + m.CSSVisualViewport.PageY,
		"width": maxX - minX, "height": maxY - minY, "scale": 1,
	}, nil
}

func writeBase64(file, data string) error {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return err
	}
	return os.WriteFile(file, raw, 0o644)
}
