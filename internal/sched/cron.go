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
// expression (minute hour day-of-month month day-of-week), in local time.
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
	t := after.Truncate(time.Minute).Add(time.Minute)
	for limit := after.AddDate(5, 0, 0); t.Before(limit); {
		switch {
		case !months[int(t.Month())]:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
		case !dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
		case !hours[t.Hour()]:
			t = t.Truncate(time.Hour).Add(time.Hour)
		case !minutes[t.Minute()]:
			t = t.Add(time.Minute)
		default:
			return t, nil
		}
	}
	return time.Time{}, errors.New("cron expression never fires")
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
