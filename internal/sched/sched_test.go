package sched

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNextCron(t *testing.T) {
	cases := []struct{ expr, after, want string }{
		{"0 7 * * 1-5", "2026-11-13 08:00", "2026-11-16 07:00"}, // Friday after 7 -> Monday
		{"*/15 * * * *", "2026-11-11 10:07", "2026-11-11 10:15"},
		{"0 0 1 * *", "2026-11-11 10:07", "2026-12-01 00:00"},
		{"30 9 * jan mon", "2026-11-11 10:07", "2027-01-04 09:30"},
		{"0 12 13 * fri", "2026-11-11 10:07", "2026-11-13 12:00"}, // dom OR dow
		{"@daily", "2026-12-31 23:59", "2027-01-01 00:00"},
		{"0 0 29 2 *", "2026-11-11 10:07", "2028-02-29 00:00"},
		{"5 4 * * 7", "2026-11-11 10:07", "2026-11-15 04:05"}, // 7 is Sunday
	}
	for _, c := range cases {
		got, err := nextCron(c.expr, at(c.after))
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if !got.Equal(at(c.want)) {
			t.Errorf("%s after %s = %s, want %s", c.expr, c.after, got.Format("2006-01-02 15:04"), c.want)
		}
	}
	for _, bad := range []string{"", "* * * *", "61 * * * *", "* * 0 * *", "a b c d e", "*/0 * * * *", "0 0 31 2 *"} {
		if _, err := nextCron(bad, at("2026-11-11 10:07")); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestParse(t *testing.T) {
	now := at("2026-11-11 10:00")
	cases := []struct{ when, wantNext, wantForm string }{
		{"every 2h", "2026-11-11 12:00", "every 2h"},
		{"every 1d12h", "2026-11-12 22:00", "every 1d12h"},
		{"in 30m", "2026-11-11 10:30", ""},
		{"in 2w", "2026-11-25 10:00", ""},
		{"at 2026-12-01T09:00", "2026-12-01 09:00", ""},
		{"at 09:00", "2026-11-12 09:00", ""},
		{"at 11:00", "2026-11-11 11:00", ""},
		{"cron 0 7 * * *", "2026-11-12 07:00", "cron 0 7 * * *"},
		{"now", "2026-11-11 10:00", ""},
	}
	for _, c := range cases {
		next, form, err := Parse(c.when, now)
		if err != nil {
			t.Errorf("%s: %v", c.when, err)
			continue
		}
		if !next.Equal(at(c.wantNext)) {
			t.Errorf("%s next = %s, want %s", c.when, next.Format("2006-01-02 15:04"), c.wantNext)
		}
		if c.wantForm != "" && form != c.wantForm {
			t.Errorf("%s stored as %q, want %q", c.when, form, c.wantForm)
		}
	}
	for _, bad := range []string{"every 10s", "in -5m", "tomorrow", "every", "at lunchtime", "in 0m"} {
		if _, _, err := Parse(bad, now); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestSchedulerFiresAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	var saved []byte
	write := func(b []byte) error { saved = b; return nil }
	var mu sync.Mutex
	var fired []string
	done := make(chan struct{}, 8)
	s, err := New(path, write, func(tr Trigger) {
		mu.Lock()
		fired = append(fired, tr.Target)
		mu.Unlock()
		done <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	clock := at("2026-11-11 10:00")
	s.now = func() time.Time { return clock }

	once, err := s.Add("chat:a", "in 30m", "ping")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("task:b", "every 1h", ""); err != nil {
		t.Fatal(err)
	}
	if len(saved) == 0 {
		t.Fatal("schedule was not persisted")
	}

	s.tick()
	if len(fired) != 0 {
		t.Fatalf("fired too early: %v", fired)
	}
	clock = at("2026-11-11 10:31")
	s.tick()
	<-done
	clock = at("2026-11-11 11:01")
	s.tick()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 2 || fired[0] != "chat:a" || fired[1] != "task:b" {
		t.Fatalf("fired = %v", fired)
	}
	left := s.List("")
	if len(left) != 1 || left[0].Target != "task:b" {
		t.Fatalf("one-shot should be gone and interval kept, got %+v", left)
	}
	if want := at("2026-11-11 12:01"); !left[0].Next.Equal(want) {
		t.Fatalf("interval rescheduled to %s, want %s", left[0].Next, want)
	}
	if s.Remove(once.ID) {
		t.Fatal("removing a fired one-shot should report false")
	}
	if !s.RemoveTarget("task:b") || len(s.List("")) != 0 {
		t.Fatal("RemoveTarget failed")
	}
}

func TestMissedTriggerFiresOnceAtStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	done := make(chan Trigger, 4)
	s, _ := New(path, func([]byte) error { return nil }, func(tr Trigger) { done <- tr })
	clock := at("2026-11-11 10:00")
	s.now = func() time.Time { return clock }
	s.Add("task:daily", "cron 0 7 * * *", "")
	clock = at("2026-11-14 09:00") // three firings were missed while off
	s.tick()
	<-done
	select {
	case <-done:
		t.Fatal("a missed trigger must fire once, not once per missed slot")
	case <-time.After(50 * time.Millisecond):
	}
	if next := s.List("")[0].Next; !next.Equal(at("2026-11-15 07:00")) {
		t.Fatalf("next = %s", next)
	}
}

// Timers stop while the computer sleeps. Run must still notice, by the wall
// clock and within a minute of waking, a trigger that came due meanwhile.
func TestRunFiresByWallClockAfterSleep(t *testing.T) {
	fired := make(chan Trigger, 1)
	s, err := New(filepath.Join(t.TempDir(), "schedule.json"), func([]byte) error { return nil }, func(tr Trigger) { fired <- tr })
	if err != nil {
		t.Fatal(err)
	}
	if now := s.now(); strings.Contains(now.String(), "m=") {
		t.Fatalf("the clock %s carries a monotonic reading, which stops during sleep", now)
	}
	if s.poll > time.Minute {
		t.Fatalf("Run sleeps up to %s at a time", s.poll)
	}

	var mu sync.Mutex
	clock := at("2026-11-11 10:00")
	s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	s.poll = 10 * time.Millisecond
	if _, err := s.Add("task:a", "in 2h", ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	clock = clock.Add(3 * time.Hour) // the wall clock moves on; the timers did not
	mu.Unlock()
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("a trigger that came due during sleep did not fire")
	}
}

// A failed save must not lose a firing: the change stays in memory, is
// reported once and is retried until it succeeds.
func TestSaveFailureIsKeptAndRetried(t *testing.T) {
	var saved string
	broken := false
	write := func(b []byte) error {
		if broken {
			return errors.New("disk full")
		}
		saved = string(b)
		return nil
	}
	fired := make(chan Trigger, 4)
	s, err := New(filepath.Join(t.TempDir(), "schedule.json"), write, func(tr Trigger) { fired <- tr })
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 4)
	s.OnError = func(err error) { errs <- err }
	clock := at("2026-11-11 10:00")
	s.now = func() time.Time { return clock }
	quiet := func() {
		t.Helper()
		select {
		case err := <-errs:
			t.Fatalf("unexpected report: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}

	if _, err := s.Add("task:once", "in 30m", ""); err != nil {
		t.Fatal(err)
	}
	broken = true
	if _, err := s.Add("task:lost", "in 1h", ""); err == nil {
		t.Fatal("Add must report a failed save")
	}
	if l := s.List(""); len(l) != 1 || l[0].Target != "task:once" {
		t.Fatalf("a trigger that could not be saved must not be scheduled: %+v", l)
	}
	quiet() // Add returned the error itself

	clock = at("2026-11-11 10:31")
	s.tick()
	<-fired // the one-shot fires although its removal could not be saved
	if err := <-errs; !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("OnError got %v", err)
	}
	if len(s.List("")) != 0 {
		t.Fatal("the fired one-shot must stay gone in memory")
	}
	s.tick() // still failing: retried, not reported again
	quiet()
	broken = false
	s.tick()
	if strings.Contains(saved, "task:once") {
		t.Fatalf("the retried save should drop the fired one-shot:\n%s", saved)
	}

	every, _ := s.Add("task:every", "every 1h", "")
	broken = true
	if !s.Remove(every.ID) {
		t.Fatal("Remove failed")
	}
	<-errs
	broken = false
	s.tick()
	if strings.Contains(saved, "task:every") {
		t.Fatalf("the retried save should drop the removed trigger:\n%s", saved)
	}
	select {
	case tr := <-fired:
		t.Fatalf("%s fired", tr.Target)
	default:
	}
}

func TestDescribe(t *testing.T) {
	for when, want := range map[string]string{
		"cron 0 7 * * 1-5": "weekdays at 07:00",
		"cron 30 18 * * *": "daily at 18:30",
		"every 2h":         "every 2h",
		"cron */5 * * * *": "cron */5 * * * *",
	} {
		if got := Describe(when); got != want {
			t.Errorf("Describe(%q) = %q, want %q", when, got, want)
		}
	}
}

func TestDurationsAndPastTimes(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30m": 30 * time.Minute, "1h1d": 25 * time.Hour, "2h3w": 506 * time.Hour, "1.5d": 36 * time.Hour,
		"1d12h": 36 * time.Hour, "1w": 168 * time.Hour, "1m30s": 90 * time.Second, "2 h": 2 * time.Hour,
	} {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1mo", "d", "1x", "-1h", "0m"} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("parseDuration(%q) should fail", bad)
		}
	}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	if _, _, err := Parse("at 2020-01-01", now); err == nil {
		t.Error("a time in the past must be refused")
	}
	if _, _, err := Parse("at 2026-11-11T12:30", now); err != nil {
		t.Errorf("a time later today: %v", err)
	}
}
