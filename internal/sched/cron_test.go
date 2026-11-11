package sched

import (
	"testing"
	"time"
	_ "time/tzdata" // the zones below, on systems without a zone database
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// Hours and days start on the wall clock, also where the UTC offset is not a
// whole number of hours.
func TestNextCronOddOffsets(t *testing.T) {
	for _, name := range []string{"Europe/Berlin", "Asia/Kolkata", "Asia/Kathmandu", "Australia/Adelaide", "America/St_Johns"} {
		loc := zone(t, name)
		on := func(month time.Month, day, hour, min int) time.Time {
			return time.Date(2026, month, day, hour, min, 0, 0, loc)
		}
		after := on(11, 11, 5, 10) // a Wednesday
		for expr, want := range map[string]time.Time{
			"0 7 * * *":    on(11, 11, 7, 0),
			"45 7 * * *":   on(11, 11, 7, 45),
			"15 9 * * 1-5": on(11, 11, 9, 15),
			"*/20 * * * *": on(11, 11, 5, 20),
			"0 0 * * sat":  on(11, 14, 0, 0),
			"0 4 1 * *":    on(12, 1, 4, 0),
		} {
			if got, err := nextCron(expr, after); err != nil || !got.Equal(want) {
				t.Errorf("%s: %q = %s, %v; want %s", name, expr, got.In(loc), err, want)
			}
		}
	}
}

// A wall-clock time that occurs twice when the clocks go back fires once, at
// its first occurrence; one that the clocks skip fires once, when they jump.
func TestNextCronDaylightSaving(t *testing.T) {
	berlin, newYork, adelaide := zone(t, "Europe/Berlin"), zone(t, "America/New_York"), zone(t, "Australia/Adelaide")
	cases := []struct {
		loc        *time.Location
		expr, from string
		want       []string // successive firings
	}{
		// 25 Oct 2026: Berlin goes from 03:00 CEST back to 02:00 CET at 01:00 UTC.
		{berlin, "30 2 * * *", "2026-10-25 00:00 CEST", []string{"2026-10-25 02:30 CEST", "2026-10-26 02:30 CET"}},
		{berlin, "0 * * * *", "2026-10-25 01:30 CEST", []string{"2026-10-25 02:00 CEST", "2026-10-25 03:00 CET"}},
		// 1 Nov 2026: New York goes from 02:00 EDT back to 01:00 EST.
		{newYork, "30 1 * * *", "2026-11-01 00:00 EDT", []string{"2026-11-01 01:30 EDT", "2026-11-02 01:30 EST"}},
		// 29 Mar 2026: Berlin goes from 02:00 CET forward to 03:00 CEST.
		{berlin, "30 2 * * *", "2026-03-28 12:00 CET", []string{"2026-03-29 03:00 CEST", "2026-03-30 02:30 CEST"}},
		{berlin, "*/15 * * * *", "2026-03-29 01:50 CET", []string{"2026-03-29 03:00 CEST", "2026-03-29 03:15 CEST"}},
		// 8 Mar 2026: New York goes from 02:00 EST forward to 03:00 EDT.
		{newYork, "30 2 * * *", "2026-03-07 12:00 EST", []string{"2026-03-08 03:00 EDT", "2026-03-09 02:30 EDT"}},
		// 4 Oct 2026: Adelaide goes from 02:00 ACST (+09:30) forward to 03:00 ACDT.
		{adelaide, "30 2 * * *", "2026-10-03 12:00 ACST", []string{"2026-10-04 03:00 ACDT", "2026-10-05 02:30 ACDT"}},
	}
	const layout = "2006-01-02 15:04 MST"
	for _, c := range cases {
		after, err := time.ParseInLocation(layout, c.from, c.loc)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range c.want {
			next, err := nextCron(c.expr, after)
			if got := next.In(c.loc).Format(layout); err != nil || got != want {
				t.Errorf("%s %q after %s = %s, %v; want %s", c.loc, c.expr, after.Format(layout), got, err, want)
				break
			}
			after = next
		}
	}
}
