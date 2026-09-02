// Package schedule parses a task schedule — an interval ("30m", "2h", "1d") or a
// standard 5-field cron expression ("0 8 * * *") — and computes the next fire
// time. It is the one place arco reasons about "when does this recurring task run
// next", used by both the scheduler loop and /schedule setup.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	cron "github.com/robfig/cron/v3"
)

// Spec is a parsed schedule.
type Spec struct {
	raw      string
	interval time.Duration // >0 for interval specs
	cron     cron.Schedule // non-nil for cron specs
}

// Parse accepts an interval ("30m", "2h", "1d", "90s") or a standard 5-field cron
// expression ("0 8 * * *"). A leading "every " is tolerated ("every 30m").
func Parse(s string) (Spec, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "every ")
	s = strings.TrimSpace(s)
	if s == "" {
		return Spec{}, fmt.Errorf("schedule: empty")
	}
	if d, ok := parseInterval(s); ok {
		if d < time.Minute {
			return Spec{}, fmt.Errorf("schedule: interval %q is too short (minimum 1m)", s)
		}
		return Spec{raw: s, interval: d}, nil
	}
	sched, err := cron.ParseStandard(s)
	if err != nil {
		return Spec{}, fmt.Errorf("schedule: %q is not an interval (e.g. 30m, 2h, 1d) or cron (e.g. 0 8 * * *)", s)
	}
	return Spec{raw: s, cron: sched}, nil
}

// Next returns the first fire time strictly after `after`.
func (sp Spec) Next(after time.Time) time.Time {
	if sp.interval > 0 {
		return after.Add(sp.interval)
	}
	return sp.cron.Next(after)
}

// Canonical returns the normalized spec string to store.
func (sp Spec) Canonical() string { return sp.raw }

// IsInterval reports whether this is an interval (vs cron) spec.
func (sp Spec) IsInterval() bool { return sp.interval > 0 }

// parseInterval parses "30m", "2h", "1d", "1w", "90s". Go's time.ParseDuration
// handles s/m/h; we add d (day) and w (week).
func parseInterval(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	unit := s[len(s)-1]
	if unit == 'd' || unit == 'w' {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil || n <= 0 {
			return 0, false
		}
		mult := 24 * time.Hour
		if unit == 'w' {
			mult = 7 * 24 * time.Hour
		}
		return time.Duration(n) * mult, true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
