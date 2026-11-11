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
		var err error
		if t, err = s.newTab(ctx, "about:blank"); err != nil {
			return nil, err
		}
	}
	select {
	case <-t.ready:
		return t, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// newTab opens a tab, makes it current and waits until it is initialised.
func (s *session) newTab(ctx context.Context, url string) (*tab, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := s.root.call(ctx, "Target.createTarget", obj{"url": url}, &created); err != nil {
		return nil, err
	}
	var t *tab
	err := s.wait(ctx, func() bool {
		for _, candidate := range s.tabs {
			if candidate.targetID == created.TargetID {
				t = candidate
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("new tab did not attach: %w", err)
	}
	select {
	case <-t.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.selectTab(ctx, t)
	return t, nil
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
	if _, err := c.s.newTab(c.ctx, "about:blank"); err != nil {
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
