package browser

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// currentTab returns the tab commands act on, opening one if none is left.
func (s *session) currentTab(ctx context.Context) (*tab, error) {
	s.mu.Lock()
	t := s.current
	s.mu.Unlock()
	if t == nil {
		return s.newTab(ctx)
	}
	select {
	case <-t.ready:
		return t, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// newTab opens a tab and makes it current.
func (s *session) newTab(ctx context.Context) (*tab, error) {
	t, err := s.openTab(ctx, false)
	if err == nil {
		s.selectTab(ctx, t)
	}
	return t, err
}

// openTab creates a tab on about:blank and waits until it is initialised
// and its first document is in place. Helper tabs stay out of the tab list.
func (s *session) openTab(ctx context.Context, helper bool) (*tab, error) {
	count := func(delta int) {
		if helper {
			s.mu.Lock()
			s.helpers += delta
			s.mu.Unlock()
		}
	}
	count(1)
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := s.root.call(ctx, "Target.createTarget", obj{"url": "about:blank", "background": helper}, &created); err != nil {
		count(-1)
		return nil, err
	}
	var t *tab
	err := s.wait(ctx, func() bool {
		for _, list := range [][]*tab{s.tabs, s.helperTabs} {
			for _, candidate := range list {
				if candidate.targetID == created.TargetID {
					t = candidate
				}
			}
		}
		return t != nil
	})
	if err != nil {
		return nil, fmt.Errorf("new tab did not attach: %w", err)
	}
	select {
	case <-t.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	t.awaitFirstDocument(ctx)
	return t, nil
}

// awaitFirstDocument waits, briefly, until the tab shows the document it was
// created for. A tab can be attached while that first navigation is still
// under way; commands sent in that window race with it, and Chromium has
// been seen to then never report the commit of the navigation that follows.
func (t *tab) awaitFirstDocument(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	t.s.wait(ctx, func() bool {
		f := t.main()
		return t.closed || (f != nil && f.url != "" && !f.loading)
	})
}

// selectTab makes t the current tab and brings it to the front.
func (s *session) selectTab(ctx context.Context, t *tab) {
	s.mu.Lock()
	s.current = t
	s.opened = removeTab(s.opened, t)
	s.notify()
	s.mu.Unlock()
	t.sess.call(ctx, "Page.bringToFront", nil, nil)
	s.syncScreencast()
}

// tabList returns the user-visible tabs and the index of the current one.
func (s *session) tabList() ([]*tab, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := -1
	for i, t := range s.tabs {
		if t == s.current {
			current = i
		}
	}
	return append([]*tab(nil), s.tabs...), current
}

func removeTab(tabs []*tab, t *tab) []*tab {
	out := tabs[:0]
	for _, other := range tabs {
		if other != t {
			out = append(out, other)
		}
	}
	return out
}

func cmdTabList(c *command) error {
	c.out.tabs = true
	return nil
}

func cmdTabNew(c *command) error {
	if _, err := c.s.newTab(c.ctx); err != nil {
		return err
	}
	c.out.say("Opened a new tab")
	if raw := c.inv.arg(0); raw != "" {
		return c.navigate(raw)
	}
	c.out.snapshot, c.out.tabs = true, true
	return nil
}

// tabArg resolves a tab index argument.
func (c *command) tabArg(arg string) (*tab, error) {
	tabs, _ := c.s.tabList()
	i, err := strconv.Atoi(arg)
	if err != nil || i < 0 || i >= len(tabs) {
		return nil, fmt.Errorf("there is no tab %q; `tab-list` shows the open tabs (0 to %d)", arg, len(tabs)-1)
	}
	return tabs[i], nil
}

func cmdTabSelect(c *command) error {
	t, err := c.tabArg(c.inv.arg(0))
	if err != nil {
		return err
	}
	c.s.selectTab(c.ctx, t)
	c.out.say("Switched to tab %s", c.inv.arg(0))
	c.out.snapshot, c.out.tabs = true, true
	return nil
}

func cmdTabClose(c *command) error {
	t, err := c.s.currentTab(c.ctx)
	if c.inv.arg(0) != "" {
		t, err = c.tabArg(c.inv.arg(0))
	}
	if err != nil {
		return err
	}
	if err := c.s.root.call(c.ctx, "Target.closeTarget", obj{"targetId": t.targetID}, nil); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	if err := c.s.wait(ctx, func() bool { return t.closed }); err != nil {
		return errors.New("the tab did not close; a beforeunload handler or dialog may be holding it open")
	}
	c.out.say("Closed the tab")
	c.out.tabs = true
	if tabs, _ := c.s.tabList(); len(tabs) > 0 {
		c.out.snapshot = true
	}
	return nil
}
