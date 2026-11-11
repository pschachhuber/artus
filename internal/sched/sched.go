// Package sched fires triggers: cron expressions, intervals and one-shot
// times that wake a task or a chat.
package sched

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Trigger wakes a target at Next.
type Trigger struct {
	ID      string    `json:"id"`
	Target  string    `json:"target"` // "task:<id>" or "chat:<id>"
	When    string    `json:"when"`   // "cron 0 7 * * 1-5" | "every 2h" | "at 2026-12-01T09:00:00+01:00"
	Note    string    `json:"note,omitempty"`
	Next    time.Time `json:"next"`
	Last    time.Time `json:"last"`
	Created time.Time `json:"created"`
}

// Scheduler keeps triggers in a JSON file and fires them when due.
type Scheduler struct {
	// OnError, if set before Run, is called on its own goroutine when the
	// schedule cannot be saved after a trigger fires or is removed. The change
	// stays in memory and Run retries the save every minute; OnError is not
	// called again until a save has succeeded.
	OnError func(error)

	write func([]byte) error
	fire  func(Trigger)
	now   func() time.Time
	poll  time.Duration // the longest Run sleeps

	mu       sync.Mutex
	triggers []Trigger
	dirty    bool // the last save failed
	changed  chan struct{}
}

// New loads the triggers stored at path. write persists the file and fire is
// called, on its own goroutine, each time a trigger comes due.
func New(path string, write func([]byte) error, fire func(Trigger)) (*Scheduler, error) {
	s := &Scheduler{write: write, fire: fire, poll: time.Minute, changed: make(chan struct{}, 1)}
	// Triggers are due by the wall clock: drop the monotonic reading, which
	// stops while the computer sleeps.
	s.now = func() time.Time { return time.Now().Round(0) }
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.triggers); err != nil {
			return nil, fmt.Errorf("schedule: %w", err)
		}
	}
	return s, nil
}

// save writes the triggers. The caller holds s.mu.
func (s *Scheduler) save() error {
	b, err := json.MarshalIndent(s.triggers, "", "  ")
	if err == nil {
		err = s.write(append(b, '\n'))
	}
	s.dirty = err != nil
	return err
}

// keep saves a change it cannot hand back, such as a firing: if the save
// fails the change stays in memory, OnError hears of it and Run retries.
// The caller holds s.mu.
func (s *Scheduler) keep() {
	reported := s.dirty
	if err := s.save(); err != nil && !reported && s.OnError != nil {
		go s.OnError(fmt.Errorf("cannot save the schedule: %w", err))
	}
}

// Add creates a trigger. when is "cron <expr>", "every <duration>",
// "in <duration>" or "at <time>". If the trigger cannot be saved, Add returns
// the error and schedules nothing.
func (s *Scheduler) Add(target, when, note string) (Trigger, error) {
	now := s.now()
	next, normalized, err := Parse(when, now)
	if err != nil {
		return Trigger{}, err
	}
	id := make([]byte, 4)
	rand.Read(id)
	t := Trigger{ID: "t" + hex.EncodeToString(id), Target: target, When: normalized, Note: note, Next: next, Created: now}
	s.mu.Lock()
	defer s.mu.Unlock()
	dirty := s.dirty
	s.triggers = append(s.triggers, t)
	if err := s.save(); err != nil {
		s.triggers, s.dirty = s.triggers[:len(s.triggers)-1], dirty
		return Trigger{}, err
	}
	select {
	case s.changed <- struct{}{}:
	default:
	}
	return t, nil
}

// Remove deletes a trigger by id.
func (s *Scheduler) Remove(id string) bool {
	return s.remove(func(t Trigger) bool { return t.ID == id })
}

// RemoveTarget deletes every trigger of a target.
func (s *Scheduler) RemoveTarget(target string) bool {
	return s.remove(func(t Trigger) bool { return t.Target == target })
}

func (s *Scheduler) remove(match func(Trigger) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.triggers[:0:0]
	for _, t := range s.triggers {
		if !match(t) {
			kept = append(kept, t)
		}
	}
	removed := len(kept) != len(s.triggers)
	if removed {
		s.triggers = kept
		s.keep()
	}
	return removed
}

// List returns the triggers, soonest first. An empty target lists all.
func (s *Scheduler) List(target string) []Trigger {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Trigger{}
	for _, t := range s.triggers {
		if target == "" || t.Target == target {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Next.Before(out[j].Next) })
	return out
}

// Run fires due triggers until ctx ends. Triggers that came due while Artus
// was not running fire once at start. Timers stop while the computer sleeps,
// so Run wakes at least every minute to check the wall clock (and to retry a
// failed save).
func (s *Scheduler) Run(ctx context.Context) {
	for {
		wait := min(s.tick(), s.poll)
		select {
		case <-ctx.Done():
			return
		case <-s.changed:
		case <-time.After(wait):
		}
	}
}

// tick fires everything that is due and returns how long to sleep.
func (s *Scheduler) tick() time.Duration {
	s.mu.Lock()
	now := s.now()
	var due []Trigger
	kept := s.triggers[:0:0]
	wait := time.Hour
	for _, t := range s.triggers {
		if !t.Next.After(now) {
			t.Last = now
			due = append(due, t)
			next, _, err := Parse(t.When, now)
			if err != nil || strings.HasPrefix(t.When, "at ") {
				continue // one-shot, done
			}
			t.Next = next
		}
		if d := t.Next.Sub(now); d < wait {
			wait = d
		}
		kept = append(kept, t)
	}
	if len(due) > 0 || s.dirty {
		s.triggers = kept
		s.keep()
	}
	s.mu.Unlock()
	for _, t := range due {
		go s.fire(t)
	}
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

// Parse validates a schedule expression and returns its next firing time
// after now, plus the form to store ("in" becomes an absolute "at").
func Parse(when string, now time.Time) (next time.Time, normalized string, err error) {
	kind, spec, _ := strings.Cut(strings.TrimSpace(when), " ")
	spec = strings.TrimSpace(spec)
	switch strings.ToLower(kind) {
	case "cron":
		next, err = nextCron(spec, now)
		return next, "cron " + spec, err
	case "every":
		d, err := parseDuration(spec)
		if err != nil {
			return next, "", err
		}
		if d < time.Minute {
			return next, "", errors.New("the shortest interval is 1m")
		}
		return now.Add(d), "every " + spec, nil
	case "in":
		d, err := parseDuration(spec)
		if err != nil {
			return next, "", err
		}
		next = now.Add(d)
		return next, "at " + next.Format(time.RFC3339), nil
	case "at":
		next, err = parseTime(spec, now)
		if err != nil {
			return next, "", err
		}
		if next.Before(now.Add(-time.Minute)) {
			return next, "", fmt.Errorf("%s has already passed (it is now %s)", next.Format("2006-01-02 15:04"), now.Format("2006-01-02 15:04"))
		}
		return next, "at " + next.Format(time.RFC3339), nil
	case "now":
		return now, "at " + now.Format(time.RFC3339), nil
	}
	return next, "", fmt.Errorf(`cannot read %q: use "cron <m h dom mon dow>", "every <2h>", "in <30m>", "at <2026-12-01T09:00>" or "now"`, when)
}

// parseDuration reads durations such as 30m, 2h, 1d, 1w, 1d12h or 1.5h.
func parseDuration(s string) (time.Duration, error) {
	rest := strings.ReplaceAll(strings.ToLower(s), " ", "")
	var total time.Duration
	for rest != "" {
		m := durationPart.FindStringSubmatch(rest)
		if m == nil {
			return 0, fmt.Errorf("cannot read duration %q: use forms like 30m, 2h, 1d, 1w", s)
		}
		n, _ := strconv.ParseFloat(m[1], 64)
		total += time.Duration(n * float64(durationUnits[m[2]]))
		rest = rest[len(m[0]):]
	}
	if total <= 0 {
		return 0, errors.New("duration must be positive")
	}
	return total, nil
}

var (
	durationPart  = regexp.MustCompile(`^(\d+(?:\.\d+)?)(ms|s|m|h|d|w)`)
	durationUnits = map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}
)

// parseTime reads an absolute time; forms without a zone are local time.
func parseTime(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("15:04", s, now.Location()); err == nil {
		next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		return next, nil
	}
	return time.Time{}, fmt.Errorf("cannot read time %q: use 2026-12-01T09:00 or 09:00", s)
}
