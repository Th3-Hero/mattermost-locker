package textfmt

import (
	"testing"
	"time"
)

func TestSpanAndRelative(t *testing.T) {
	cases := []struct {
		d    time.Duration
		span string
		rel  string
	}{
		{10 * time.Second, "less than a minute", "just now"},
		{time.Minute, "1 minute", "1 minute ago"},
		{35 * time.Minute, "35 minutes", "35 minutes ago"},
		{time.Hour, "1 hour", "1 hour ago"},
		{4 * time.Hour, "4 hours", "4 hours ago"},
		{4*time.Hour + 12*time.Minute, "4 hours 12 minutes", "4 hours 12 minutes ago"},
		{24 * time.Hour, "1 day", "1 day ago"},
		{50*time.Hour + 30*time.Minute, "2 days 2 hours", "2 days 2 hours ago"},
		{24*time.Hour - 400*time.Millisecond, "1 day", "1 day ago"}, // sub-minute jitter is rounded away
		{59*time.Minute + 40*time.Second, "1 hour", "1 hour ago"},
	}
	for _, c := range cases {
		if got := Span(c.d); got != c.span {
			t.Errorf("Span(%s) = %q, want %q", c.d, got, c.span)
		}
		if got := Relative(c.d); got != c.rel {
			t.Errorf("Relative(%s) = %q, want %q", c.d, got, c.rel)
		}
	}
	if got := Relative(-20 * time.Hour); got != "in 20 hours" {
		t.Errorf("future: %q", got)
	}
	if got := Relative(-5 * time.Second); got != "in less than a minute" {
		t.Errorf("future short: %q", got)
	}
}

func TestDuration(t *testing.T) {
	cases := map[time.Duration]string{
		24 * time.Hour:             "24h",
		36 * time.Minute:           "36m",
		90 * time.Minute:           "1h30m",
		time.Hour + 10*time.Minute: "1h10m",
		10 * time.Minute:           "10m",
		30 * time.Second:           "30s",
		2*time.Hour + 5*time.Minute + 3*time.Second: "2h5m3s",
	}
	for d, want := range cases {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestWhen(t *testing.T) {
	now := time.Date(2026, 11, 11, 16, 30, 0, 0, time.UTC)
	f := Formatter{Loc: time.UTC, Now: func() time.Time { return now }}
	if got, want := f.When(now.Add(-4*time.Hour)), "2026-11-11 12:30 UTC (4 hours ago)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := f.When(now.Add(20*time.Hour)), "2026-11-12 12:30 UTC (in 20 hours)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	ny, _ := time.LoadLocation("America/New_York")
	f.Loc = ny
	if got, want := f.Time(now), "2026-11-11 11:30 EST"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
