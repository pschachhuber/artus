package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

// handoff is a pending request for the user to take over a session.
type handoff struct {
	event   Event
	done    chan struct{}
	once    sync.Once
	outcome string // resolved, timeout, cancelled or closed
}

func (h *handoff) finish(outcome string) {
	h.once.Do(func() {
		h.outcome = outcome
		close(h.done)
	})
}

// ResolveHandoff marks the hand-off with the given id as completed by the
// user, which unblocks the `handoff` command waiting on it. It reports
// whether such a hand-off was pending.
func (m *Manager) ResolveHandoff(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		s.mu.Lock()
		h := s.handoff
		s.mu.Unlock()
		if h != nil && h.event.ID == id {
			h.finish("resolved")
			return true
		}
	}
	return false
}

// cmdHandoff asks the user to take over: it announces the request, waits
// until the user reports back, the timeout passes or the command is
// cancelled, and then shows the page as the user left it. Other commands may
// run on the session meanwhile.
func cmdHandoff(c *command) error {
	s := c.s
	t, err := s.currentTab(c.ctx)
	if err != nil {
		return err
	}
	timeout, err := c.inv.timeout(c.cfg.handoffTimeout())
	if err != nil {
		return err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	reason := strings.Join(c.inv.args, " ")
	url, _ := t.urlAndTitle(c.ctx)
	h := &handoff{
		event: Event{Type: "handoff", Session: s.name, ID: hex.EncodeToString(id[:]), Reason: reason, URL: url},
		done:  make(chan struct{}),
	}
	s.mu.Lock()
	if s.handoff != nil {
		s.mu.Unlock()
		return errors.New("a hand-off is already pending for this session")
	}
	s.handoff = h
	s.mu.Unlock()

	t.sess.call(c.ctx, "Page.bringToFront", nil, nil)
	// A user at a headed browser should get the real file picker.
	s.interceptChoosers(s.headless)
	c.m.emit(h.event)

	s.cmdMu.Unlock()
	timer := time.NewTimer(timeout)
	select {
	case <-h.done:
	case <-timer.C:
		h.finish("timeout")
	case <-c.ctx.Done():
		h.finish("cancelled")
	}
	timer.Stop()
	s.cmdMu.Lock()

	s.mu.Lock()
	s.handoff = nil
	s.mu.Unlock()
	c.m.emit(Event{Type: "handoff_done", Session: s.name, ID: h.event.ID, Reason: h.outcome})
	if h.outcome != "closed" {
		s.interceptChoosers(true)
	}
	switch h.outcome {
	case "cancelled":
		return c.ctx.Err()
	case "closed":
		return errors.New("the browser was closed during the hand-off")
	case "timeout":
		c.out.say("The hand-off timed out after %s without the user confirming. The page is shown as it is now.", timeout)
	default:
		c.out.say("The user completed the hand-off (%s).", reason)
	}
	if current, err := s.currentTab(c.ctx); err == nil {
		current.settle(c.ctx, time.Now())
	}
	c.out.snapshot = true
	return nil
}

// interceptChoosers switches file chooser interception on all tabs.
func (s *session) interceptChoosers(enabled bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tabs, _ := s.tabList()
	for _, t := range tabs {
		t.sess.call(ctx, "Page.setInterceptFileChooserDialog", obj{"enabled": enabled}, nil)
	}
}
