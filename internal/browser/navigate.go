package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// settleQuiet is how long a page must show no network or navigation
	// activity after an action before it counts as settled.
	settleQuiet = 150 * time.Millisecond
	// settleRequests bounds the wait for requests and downloads an action
	// started.
	settleRequests = 5 * time.Second
	// settleLoad bounds the wait for the load event once a navigation has
	// committed; the page is usable before that.
	settleLoad = 5 * time.Second
	// settleStalled bounds the wait for a requested navigation that never
	// starts loading, such as a link to an external protocol handler.
	settleStalled = 3 * time.Second
)

// guard returns a context that ends when a JavaScript dialog opens or the tab
// closes: page scripts, and most protocol calls with them, stall until the
// dialog is handled.
func (t *tab) guard(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t.s.wait(ctx, func() bool { return t.dialog != nil || t.closed })
		cancel()
	}()
	return ctx, cancel
}

// beginAction starts tracking the network requests an action causes.
func (t *tab) beginAction() time.Time {
	t.s.mu.Lock()
	t.inflight = map[string]bool{}
	t.s.mu.Unlock()
	return time.Now()
}

// settle waits for the consequences of an action that began at since: a
// navigation it started commits and loads, requests it fired complete, and
// the page stays quiet for a moment. Every wait is bounded; settle never
// fails.
func (t *tab) settle(ctx context.Context, since time.Time) {
	s := t.s
	_, navTimeout := s.timeouts()
	t.flushTasks(ctx)
	settleStart := time.Now()
	for {
		now := time.Now()
		var wait time.Duration
		pick := func(active bool, remaining time.Duration) {
			if wait == 0 && active && remaining > 0 {
				wait = remaining
			}
		}
		s.mu.Lock()
		if f := t.main(); f != nil && !t.closed && t.dialog == nil && !s.closing {
			quietSince := settleStart
			for _, at := range []time.Time{t.activity, s.downloadActivity} {
				if at.After(quietSince) {
					quietSince = at
				}
			}
			pick(f.pendingNav && f.loading, navTimeout-now.Sub(f.requested))
			pick(f.pendingNav && !f.loading, settleStalled-now.Sub(f.requested))
			pick(f.committed.After(since) && f.loading && !f.loaded("load"), settleLoad-now.Sub(f.committed))
			pick(len(t.inflight) > 0 || len(s.downloading()) > 0, settleRequests-now.Sub(since))
			pick(true, settleQuiet-now.Sub(quietSince))
		}
		changed := s.changed
		s.mu.Unlock()
		if wait == 0 {
			return
		}
		timer := time.NewTimer(wait)
		select {
		case <-changed:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		timer.Stop()
	}
}

// flushTasks gives the page's event loop a turn, so that work an input event
// queued (a form submission, a scripted navigation) has started before we
// look for its effects.
func (t *tab) flushTasks(ctx context.Context) {
	ctx, cancel := t.guard(ctx)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	t.sess.call(ctx, "Runtime.evaluate", obj{
		"expression":   "new Promise(resolve => setTimeout(resolve, 0))",
		"awaitPromise": true,
	}, nil)
}

// navigate loads url in the tab and waits until the new document is parsed.
func (t *tab) navigate(ctx context.Context, url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	var res struct {
		LoaderID   string `json:"loaderId"`
		ErrorText  string `json:"errorText"`
		IsDownload bool   `json:"isDownload"`
	}
	err := t.sess.call(ctx, "Page.navigate", obj{"url": url}, &res)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("navigation to %s timed out after %s: no response arrived", url, timeout)
	case err != nil:
		return err
	case res.ErrorText != "":
		t.s.mu.Lock()
		download := res.IsDownload || t.s.downloadActivity.After(start)
		t.s.mu.Unlock()
		if download {
			return nil
		}
		return fmt.Errorf("navigation to %s failed: %s", url, res.ErrorText)
	case res.LoaderID == "":
		return nil // navigation within the current document
	}
	lost := false
	err = t.waitParsed(ctx, url, timeout, func() bool {
		f := t.main()
		// Should the commit go unreported (see awaitFirstDocument), the
		// lifecycle events of the new document still give it away.
		lost = f.loaderID != res.LoaderID && (f.lifecycle[res.LoaderID+"DOMContentLoaded"] || f.lifecycle[res.LoaderID+"networkAlmostIdle"])
		return lost || f.committed.After(start) && f.loaded("DOMContentLoaded")
	})
	if lost && err == nil {
		err = t.resync(ctx)
	}
	return err
}

// resync reloads the frame tree from the browser after an event that should
// have updated it was missed.
func (t *tab) resync(ctx context.Context) error {
	var tree struct {
		FrameTree frameTreeNode `json:"frameTree"`
	}
	if err := t.sess.call(ctx, "Page.getFrameTree", nil, &tree); err != nil {
		return err
	}
	s := t.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if tg := s.targets[t.sess.id]; tg != nil {
		f := t.frame(tree.FrameTree.Frame.ID)
		t.setFrameInfo(f, tg, tree.FrameTree.Frame)
		f.committed, f.pendingNav = time.Now(), false
		t.navSeq++
		s.notify()
	}
	return nil
}

// history moves delta entries through the session history.
func (t *tab) history(ctx context.Context, delta int, timeout time.Duration) error {
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID  int    `json:"id"`
			URL string `json:"url"`
		} `json:"entries"`
	}
	if err := t.sess.call(ctx, "Page.getNavigationHistory", nil, &h); err != nil {
		return err
	}
	i := h.CurrentIndex + delta
	if i < 0 || i >= len(h.Entries) {
		if delta < 0 {
			return errors.New("there is no previous page in this tab's history")
		}
		return errors.New("there is no next page in this tab's history")
	}
	return t.navigateBy(ctx, h.Entries[i].URL, timeout, "Page.navigateToHistoryEntry", obj{"entryId": h.Entries[i].ID})
}

// reload reloads the current document.
func (t *tab) reload(ctx context.Context, timeout time.Duration) error {
	return t.navigateBy(ctx, t.url(), timeout, "Page.reload", nil)
}

// navigateBy issues a navigation command that does not report its own
// completion and waits for the main frame to move on and parse.
func (t *tab) navigateBy(ctx context.Context, url string, timeout time.Duration, method string, params any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t.s.mu.Lock()
	seq := t.navSeq
	t.s.mu.Unlock()
	if err := t.sess.call(ctx, method, params, nil); err != nil {
		return err
	}
	return t.waitParsed(ctx, url, timeout, func() bool { return t.navSeq != seq && t.main().loaded("DOMContentLoaded") })
}

// waitParsed waits until done, called with the main frame present, reports
// that the navigation took effect and its document has been parsed.
func (t *tab) waitParsed(ctx context.Context, url string, timeout time.Duration, done func() bool) error {
	err := t.s.wait(ctx, func() bool {
		return t.closed || t.dialog != nil || (t.main() != nil && done())
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("navigation to %s timed out after %s: the page started loading but was not parsed in time", url, timeout)
	}
	if err == nil && t.isClosed() {
		return errNoTab
	}
	return err
}

func (t *tab) isClosed() bool {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	return t.closed
}

// waitLoad waits for the main document's load event.
func (t *tab) waitLoad(ctx context.Context) error {
	return t.s.wait(ctx, func() bool {
		f := t.main()
		return t.closed || t.dialog != nil || (f != nil && f.loaded("load"))
	})
}

// normalizeURL adds a scheme to bare host names, like a browser's address bar.
func normalizeURL(raw string) string {
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "about:") || strings.HasPrefix(raw, "data:") ||
		strings.HasPrefix(raw, "file:") || strings.HasPrefix(raw, "chrome:") || strings.HasPrefix(raw, "javascript:") ||
		strings.HasPrefix(raw, "view-source:") || strings.HasPrefix(raw, "blob:") {
		return raw
	}
	host := raw
	if i := strings.IndexAny(host, "/:?#"); i >= 0 {
		host = host[:i]
	}
	if host == "localhost" || host == "127.0.0.1" || host == "0.0.0.0" || strings.HasPrefix(raw, "[::1]") {
		return "http://" + raw
	}
	return "https://" + raw
}
