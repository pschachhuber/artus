package sched

import (
	"path/filepath"
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
