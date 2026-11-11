package sched

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var cronMacros = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@weekly":  "0 0 * * 0",
	"@monthly": "0 0 1 * *",
	"@yearly":  "0 0 1 1 *",
}

var cronNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// cronField parses one field into the set of values it allows.
func cronField(field string, min, max int) (set map[int]bool, any bool, err error) {
	set = map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		rng, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			if step, err = strconv.Atoi(stepText); err != nil || step < 1 {
				return nil, false, fmt.Errorf("bad step in %q", part)
			}
		}
		lo, hi := min, max
		switch {
		case rng == "*":
			any = any || !hasStep
		default:
			from, to, isRange := strings.Cut(rng, "-")
			if lo, err = cronValue(from); err != nil {
				return nil, false, err
			}
			hi = lo
			if isRange {
				if hi, err = cronValue(to); err != nil {
					return nil, false, err
				}
			} else if hasStep {
				hi = max
			}
		}
		if lo < min || hi > max || lo > hi {
			return nil, false, fmt.Errorf("%q is outside %d-%d", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			set[v] = true
		}
	}
	return set, any, nil
}

func cronValue(s string) (int, error) {
	if n, ok := cronNames[strings.ToLower(s)]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad cron value %q", s)
	}
	return n, nil
}

// nextCron returns the first time after `after` matching a five-field cron
// expression (minute hour day-of-month month day-of-week) on the wall clock of
// after's location. As in cron, a time that occurs twice when the clocks go
// back fires once, at its first occurrence, and a time that the clocks skip
// when they go forward fires once, at the moment they jump.
func nextCron(expr string, after time.Time) (time.Time, error) {
	if m, ok := cronMacros[strings.ToLower(expr)]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return time.Time{}, errors.New("a cron expression has five fields: minute hour day-of-month month day-of-week")
	}
	minutes, _, err := cronField(f[0], 0, 59)
	if err != nil {
		return time.Time{}, err
	}
	hours, _, err := cronField(f[1], 0, 23)
	if err != nil {
		return time.Time{}, err
	}
	days, anyDay, err := cronField(f[2], 1, 31)
	if err != nil {
		return time.Time{}, err
	}
	months, _, err := cronField(f[3], 1, 12)
	if err != nil {
		return time.Time{}, err
	}
	weekdays, anyWeekday, err := cronField(f[4], 0, 7)
	if err != nil {
		return time.Time{}, err
	}
	if weekdays[7] {
		weekdays[0] = true
	}
	dayMatches := func(t time.Time) bool {
		dom, dow := days[t.Day()], weekdays[int(t.Weekday())]
		if anyDay || anyWeekday {
			return dom && dow
		}
		return dom || dow // both restricted: either may match, as in cron
	}
	// Step through wall-clock readings, held as UTC times so that every day
	// has 24 hours, and turn a match into an instant only at the end.
	c := wallClock(after).Add(time.Minute)
	for limit := c.AddDate(5, 0, 0); c.Before(limit); {
		switch {
		case !months[int(c.Month())]:
			c = time.Date(c.Year(), c.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !dayMatches(c):
			c = time.Date(c.Year(), c.Month(), c.Day()+1, 0, 0, 0, 0, time.UTC)
		case !hours[c.Hour()]:
			c = time.Date(c.Year(), c.Month(), c.Day(), c.Hour()+1, 0, 0, 0, time.UTC)
		case !minutes[c.Minute()]:
			c = c.Add(time.Minute)
		default:
			if t := instant(c, after.Location()); t.After(after) {
				return t, nil
			}
			c = c.Add(time.Minute) // the first occurrence of a repeated time has passed
		}
	}
	return time.Time{}, errors.New("cron expression never fires")
}

// wallClock returns the reading of t's clock, to the minute, as a UTC time.
func wallClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// instant returns the first moment at which the clocks in loc read c, or, if
// they skip c when going forward, the moment they jump.
func instant(c time.Time, loc *time.Location) time.Time {
	// time.Date picks either reading of a repeated time, and moves a skipped
	// one to either side of the jump.
	t := time.Date(c.Year(), c.Month(), c.Day(), c.Hour(), c.Minute(), 0, 0, loc)
	start, end := t.ZoneBounds()
	switch w := wallClock(t); {
	case w.After(c):
		return start
	case w.Before(c):
		return end
	case start.IsZero():
		return t
	}
	// If the clocks went back at start, they may have read c before it too.
	_, before := start.Add(-time.Second).Zone()
	_, offset := t.Zone()
	if first := t.Add(time.Duration(offset-before) * time.Second); first.Before(start) {
		return first
	}
	return t
}

// Describe renders a stored schedule in plain language for the interface.
func Describe(when string) string {
	kind, spec, _ := strings.Cut(when, " ")
	switch kind {
	case "every":
		return "every " + spec
	case "at":
		if t, err := time.Parse(time.RFC3339, spec); err == nil {
			return "once, " + t.Local().Format("Mon 2 Jan 2006 15:04")
		}
	case "cron":
		f := strings.Fields(spec)
		if len(f) == 5 && f[2] == "*" && f[3] == "*" {
			if h, err := strconv.Atoi(f[1]); err == nil {
				if m, err := strconv.Atoi(f[0]); err == nil {
					at := fmt.Sprintf("%02d:%02d", h, m)
					switch f[4] {
					case "*":
						return "daily at " + at
					case "1-5":
						return "weekdays at " + at
					case "0,6", "6,0":
						return "weekends at " + at
					}
					return "at " + at + " on days " + f[4]
				}
			}
		}
		return "cron " + spec
	}
	return when
}
