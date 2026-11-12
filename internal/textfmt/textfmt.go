// Package textfmt renders times and durations for humans.
package textfmt

import (
	"fmt"
	"time"
)

const timeLayout = "2006-01-02 15:04 MST"

// Formatter renders times in a fixed location relative to a clock.
type Formatter struct {
	Loc *time.Location
	Now func() time.Time
}

// Time renders t like "2026-11-11 12:30 UTC".
func (f Formatter) Time(t time.Time) string {
	loc := f.Loc
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format(timeLayout)
}

// When renders t with a relative suffix, e.g.
// "2026-11-11 12:30 UTC (4 hours ago)" or "2026-11-12 12:30 UTC (in 20 hours)".
func (f Formatter) When(t time.Time) string {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	return f.Time(t) + " (" + Relative(now().Sub(t)) + ")"
}

// Relative describes a signed offset from now: positive d means "in the past".
func Relative(d time.Duration) string {
	if d >= 0 {
		if d < time.Minute {
			return "just now"
		}
		return Span(d) + " ago"
	}
	return "in " + Span(-d)
}

// Span renders a length of time using at most two units,
// e.g. "35 minutes", "4 hours 12 minutes", "2 days 3 hours".
func Span(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return "less than a minute"
	}
	// Round to the nearest minute so that, e.g., a lock that expires in
	// 23h59m59.6s reads "1 day" rather than "23 hours 59 minutes".
	d = d.Round(time.Minute)
	switch {
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return pair(int(d/time.Hour), "hour", int(d%time.Hour/time.Minute), "minute")
	default:
		return pair(int(d/(24*time.Hour)), "day", int(d%(24*time.Hour)/time.Hour), "hour")
	}
}

// Duration renders a duration in compact config style: "24h", "36m", "1h30m".
func Duration(d time.Duration) string {
	s := d.String()
	if len(s) > 2 && s[len(s)-2:] == "0s" && s[len(s)-3] == 'm' {
		s = s[:len(s)-2]
	}
	if len(s) > 2 && s[len(s)-2:] == "0m" && s[len(s)-3] == 'h' {
		s = s[:len(s)-2]
	}
	return s
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func pair(a int, aUnit string, b int, bUnit string) string {
	if b == 0 {
		return plural(a, aUnit)
	}
	return plural(a, aUnit) + " " + plural(b, bUnit)
}
