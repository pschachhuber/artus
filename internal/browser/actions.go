package browser

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// retryBackoff is the pause before each retry of an element action; the last
// entry repeats.
var retryBackoff = []time.Duration{0, 20 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond}

const (
	// actionRelease bounds the cleanup of element handles.
	actionRelease = 5 * time.Second
	// chooserWait is how long a click may take to open a file chooser.
	chooserWait = 3 * time.Second
)

// scrollAlignments are tried in turn on retries, after the default "only if
// needed" scrolling: an element under a sticky header or footer comes out
// from behind it with a different alignment.
var scrollAlignments = []string{"end", "center", "start"}

// attempt tries an action once. A non-empty reason means the element is not
// ready yet and the action should be retried.
type attempt func(ctx context.Context, el *element, try int) (reason string, err error)

// retry resolves target and runs fn until it succeeds, fails for good, or
// the timeout passes. The error then says what the element was waiting for.
func (t *tab) retry(ctx context.Context, target string, timeout time.Duration, fn attempt) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	label, reason := target, ""
	for try := 0; ; try++ {
		if try > 0 {
			select {
			case <-time.After(retryBackoff[min(try-1, len(retryBackoff)-1)]):
			case <-ctx.Done():
			}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %s waiting for %s: %s", timeout, label, reason)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		el, err := t.resolve(ctx, target)
		if err == nil {
			label = el.label
			reason, err = fn(ctx, el, try)
			el.release()
		}
		switch {
		case err == nil && reason == "":
			return nil
		case err == nil:
		case ctx.Err() != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)):
			// Reported at the top of the loop.
		case errors.Is(err, errNoMatch):
			reason = "no element matches the selector"
		case isStale(err):
			if refPattern.MatchString(target) {
				return staleRefError(target)
			}
			reason = "the element was detached from the page"
		default:
			return err
		}
	}
}

// waitStates checks the element's actionability states, bringing the tab to
// the front when it is hidden (hidden tabs do not process input).
func (el *element) waitStates(ctx context.Context, states ...string) (string, error) {
	var st struct {
		Missing  string `json:"missing"`
		Hidden   bool   `json:"hidden"`
		Dropdown bool   `json:"dropdown"`
	}
	if err := el.call(ctx, "state", obj{"states": states}, &st); err != nil {
		return "", err
	}
	el.dropdown = st.Dropdown
	if st.Hidden {
		el.t.sess.call(ctx, "Page.bringToFront", nil, nil)
	}
	switch st.Missing {
	case "":
		return "", nil
	case "enabled":
		return "element is disabled", nil
	case "editable":
		return "element is disabled or read-only", nil
	case "stable":
		return "element is still moving", nil
	case "interactive":
		return "element is inert", nil
	}
	return "element is not visible", nil
}

// actionPoint readies the element for a pointer action and returns the
// top-level viewport point to act on: it waits for the element to be
// visible, stable and (optionally) enabled, scrolls it into view, and checks
// that the point is not covered by something else.
func (el *element) actionPoint(ctx context.Context, try int, needEnabled bool) (point, string, error) {
	states := []string{"visible", "stable"}
	if needEnabled {
		states = append(states, "enabled")
	}
	if reason, err := el.waitStates(ctx, states...); reason != "" || err != nil {
		return point{}, reason, err
	}
	objectID, err := el.object(ctx)
	if err != nil {
		return point{}, "", err
	}
	if align := try % (len(scrollAlignments) + 1); align == 0 {
		err = el.frame.sess.call(ctx, "DOM.scrollIntoViewIfNeeded", obj{"objectId": objectID}, nil)
		if cdpErrorContains(err, "layout object") {
			return point{}, "element is not visible", nil
		}
	} else {
		err = el.call(ctx, "scroll", obj{"block": scrollAlignments[align-1]}, nil)
	}
	if err != nil {
		return point{}, "", err
	}
	quads, err := contentQuads(ctx, el.frame.sess, obj{"objectId": objectID})
	if err != nil {
		return point{}, "", err
	}
	if len(quads) == 0 {
		return point{}, "element is not visible", nil
	}
	base, err := el.t.rootOrigin(ctx, el.frame)
	if err != nil {
		return point{}, "", err
	}
	w, h, err := el.t.viewportSize(ctx)
	if err != nil {
		return point{}, "", err
	}
	pt, ok := clickablePoint(quads, base, w, h)
	if !ok {
		return point{}, "element is outside of the viewport", nil
	}
	blocker, err := el.hitCheck(ctx, pt)
	if err != nil {
		return point{}, "", err
	}
	if blocker != "" {
		return point{}, "element is covered by " + blocker + ", which would receive the pointer event instead", nil
	}
	return pt, "", nil
}

// clickablePoint returns the centre of the first quad that, shifted by base
// and clipped to the viewport, still has some area.
func clickablePoint(quads [][4]point, base point, w, h float64) (point, bool) {
	for _, q := range quads {
		var cx, cy, area float64
		for i := range q {
			q[i].x = math.Min(math.Max(q[i].x+base.x, 0), w)
			q[i].y = math.Min(math.Max(q[i].y+base.y, 0), h)
		}
		for i, p := range q {
			next := q[(i+1)%4]
			area += (p.x*next.y - next.x*p.y) / 2
			cx += p.x / 4
			cy += p.y / 4
		}
		// Allow 1x1 elements; compare against 0.99 to absorb rounding.
		if math.Abs(area) > 0.99 {
			return point{math.Round(cx), math.Round(cy)}, true
		}
	}
	return point{}, false
}

// hitCheck verifies that a pointer event at pt reaches the element: inside
// its own frame it must land on the element, and in every enclosing frame on
// the iframe leading to it. It returns a description of whatever is in the
// way.
func (el *element) hitCheck(ctx context.Context, pt point) (string, error) {
	target, f := el, el.frame
	for {
		origin, owner, err := el.t.frameOrigin(ctx, f)
		if err != nil {
			return "", err
		}
		var hit struct {
			Blocker string `json:"blocker"`
		}
		if err := target.call(ctx, "hit", obj{"x": pt.x - origin.x, "y": pt.y - origin.y}, &hit); err != nil {
			return "", err
		}
		if target != el {
			target.release()
		}
		if hit.Blocker != "" || f.parentID == "" {
			return hit.Blocker, nil
		}
		parent, ok := el.t.frameRef(f.parentID)
		if !ok {
			return "", errStale
		}
		target, f = &element{t: el.t, frame: parent, backend: owner}, parent
	}
}

// clickOptions selects the mouse button, click count and held modifiers.
type clickOptions struct {
	button    string
	count     int
	modifiers int
}

// pointer readies the target for a pointer action (see actionPoint) and
// performs it at the point found.
func (t *tab) pointer(ctx context.Context, target string, timeout time.Duration, needEnabled bool, perform func(ctx context.Context, el *element, pt point) error) (label string, err error) {
	err = t.retry(ctx, target, timeout, func(ctx context.Context, el *element, try int) (string, error) {
		label = el.label
		pt, reason, err := el.actionPoint(ctx, try, needEnabled)
		if reason != "" || err != nil {
			return reason, err
		}
		return "", perform(ctx, el, pt)
	})
	return label, err
}

// click performs a real mouse click on the target once it is actionable.
func (t *tab) click(ctx context.Context, target string, timeout time.Duration, opts clickOptions) (string, error) {
	return t.pointer(ctx, target, timeout, true, func(ctx context.Context, el *element, pt point) error {
		if el.dropdown {
			// Its option list is drawn by the browser outside of the page,
			// where neither snapshots nor refs reach.
			return errors.New("this is a <select>; choose an option with `select <ref> <value or label>` instead of clicking it")
		}
		return t.mouseClick(ctx, pt, opts)
	})
}

// fill replaces the content of a text control, or sets the value of a date,
// time, colour or range input.
func (t *tab) fill(ctx context.Context, target, value string, timeout time.Duration) (string, error) {
	var label string
	err := t.retry(ctx, target, timeout, func(ctx context.Context, el *element, try int) (string, error) {
		label = el.label
		if reason, err := el.waitStates(ctx, "visible", "enabled", "editable"); reason != "" || err != nil {
			return reason, err
		}
		var res struct {
			Result string `json:"result"`
		}
		if err := el.call(ctx, "fill", obj{"value": value}, &res); err != nil {
			return "", err
		}
		if res.Result != "needsinput" {
			return "", nil
		}
		// The control is focused with its content selected: typing replaces it.
		return "", t.insertText(ctx, value)
	})
	return label, err
}

// selectOptions selects options of a <select> by value or label.
func (t *tab) selectOptions(ctx context.Context, target string, values []string, timeout time.Duration) (string, []string, error) {
	var label string
	var selected []string
	var available []string
	err := t.retry(ctx, target, timeout, func(ctx context.Context, el *element, try int) (string, error) {
		label = el.label
		if reason, err := el.waitStates(ctx, "visible", "enabled"); reason != "" || err != nil {
			return reason, err
		}
		var res struct {
			Values  []string `json:"values"`
			Retry   string   `json:"retry"`
			Options []string `json:"options"`
		}
		if err := el.call(ctx, "select", obj{"values": values}, &res); err != nil {
			return "", err
		}
		selected, available = res.Values, res.Options
		return res.Retry, nil
	})
	if err != nil && len(available) > 0 {
		err = fmt.Errorf("%w (available options: %q)", err, available)
	}
	return label, selected, err
}

// setChecked clicks a checkbox or radio button unless it already has the
// wanted state, and verifies that the click took effect.
func (t *tab) setChecked(ctx context.Context, target string, want bool, timeout time.Duration) (label string, changed bool, err error) {
	type state struct {
		Checked any  `json:"checked"`
		Radio   bool `json:"radio"`
	}
	err = t.retry(ctx, target, timeout, func(ctx context.Context, el *element, try int) (string, error) {
		label = el.label
		var before, after state
		if err := el.call(ctx, "checked", nil, &before); err != nil {
			return "", err
		}
		if before.Checked == want {
			return "", nil
		}
		if before.Radio && !want {
			return "", errors.New("a radio button cannot be unchecked; check another radio button of its group instead")
		}
		pt, reason, err := el.actionPoint(ctx, try, true)
		if reason != "" || err != nil {
			return reason, err
		}
		if err := t.mouseClick(ctx, pt, clickOptions{}); err != nil {
			return "", err
		}
		changed = true
		if err := el.call(ctx, "checked", nil, &after); isStale(err) {
			return "", nil // the click replaced the page; nothing left to verify
		} else if err != nil {
			return "", err
		}
		if after.Checked != want {
			return "", errors.New("clicking the element did not change its checked state")
		}
		return "", nil
	})
	return label, changed, err
}

// fileInputFunction returns the <input type=file> a node stands for: itself
// or the control of its label.
const fileInputFunction = `function() {
  const isFile = e => e && e.localName === 'input' && e.type === 'file';
  const element = this.nodeType === 1 ? this : this.parentElement;
  if (isFile(element)) return element;
  const label = element && element.closest('label');
  return label && isFile(label.control) ? label.control : null;
}`

// upload sets the files of a file input. When the target is not a file input
// it is clicked, and the file chooser it opens receives the files.
func (t *tab) upload(ctx context.Context, target string, files []string, timeout time.Duration) (string, error) {
	var label string
	err := t.retry(ctx, target, timeout, func(ctx context.Context, el *element, try int) (string, error) {
		label = el.label
		objectID, err := el.object(ctx)
		if err != nil {
			return "", err
		}
		var res evalResult
		err = el.frame.sess.call(ctx, "Runtime.callFunctionOn", obj{"objectId": objectID, "functionDeclaration": fileInputFunction}, &res)
		if err != nil {
			return "", err
		}
		if res.Result.ObjectID != "" {
			return "", el.frame.sess.call(ctx, "DOM.setFileInputFiles", obj{"objectId": res.Result.ObjectID, "files": files}, nil)
		}
		pt, reason, err := el.actionPoint(ctx, try, true)
		if reason != "" || err != nil {
			return reason, err
		}
		t.s.mu.Lock()
		t.chooser = nil
		t.s.mu.Unlock()
		if err := t.mouseClick(ctx, pt, clickOptions{}); err != nil {
			return "", err
		}
		wait, cancel := context.WithTimeout(ctx, chooserWait)
		defer cancel()
		if err := t.s.wait(wait, func() bool { return t.chooser != nil }); err != nil && ctx.Err() == nil {
			return "", errors.New("the element is not a file input and clicking it did not open a file chooser")
		} else if err != nil {
			return "", err
		}
		return "", t.acceptChooser(ctx, files)
	})
	return label, err
}

// acceptChooser hands files to the pending intercepted file chooser.
func (t *tab) acceptChooser(ctx context.Context, files []string) error {
	t.s.mu.Lock()
	ch := t.chooser
	t.chooser = nil
	t.s.mu.Unlock()
	if ch == nil {
		return errors.New("no file chooser is open; pass the ref of the file input: upload <ref> <file...>")
	}
	if len(files) > 1 && !ch.multiple {
		return errors.New("this file chooser accepts a single file only")
	}
	return ch.sess.call(ctx, "DOM.setFileInputFiles", obj{"backendNodeId": ch.backendID, "files": files}, nil)
}
