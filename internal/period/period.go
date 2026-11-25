// Package period provides ISO 8601 duration parsing and budget period
// window math.
//
// Budget periods are anchored at the budget's period_start and advance by a
// fixed ISO 8601 duration (period_duration, e.g. P1M, P7D). Calendar units
// (years, months) advance relative to the boundary they are added to, via
// time.AddDate normalization: a P1M period starting Jan 31 ends Feb 28 (or
// Mar 2/3 in non-leap years), matching how time.AddDate clamps. Both the
// store (computing next_renewal_at on budget create/update and advancing it
// on renewal) and the renewal package (deriving the completed period window)
// must agree on these rules — they share this package to guarantee it.
package period

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a parsed ISO 8601 duration. All components are non-negative.
type Duration struct {
	years, months, weeks, days int
	hours, minutes, seconds    int
}

// Parse parses an ISO 8601 duration of the form PnYnMnWnDTnHnMnS. At least
// one component must be present and non-zero; each unit may appear at most
// once, and date units may not appear after the 'T' separator (nor time
// units before it). Component order within each half is not enforced.
// Fractional values are not supported — budget periods are not sub-second.
func Parse(s string) (Duration, error) {
	if len(s) == 0 || s[0] != 'P' {
		return Duration{}, fmt.Errorf("invalid ISO 8601 duration %q: must start with 'P'", s)
	}
	body := s[1:]

	var datePart, timePart string
	if i := strings.IndexByte(body, 'T'); i >= 0 {
		datePart, timePart = body[:i], body[i+1:]
		if timePart == "" {
			return Duration{}, fmt.Errorf("invalid ISO 8601 duration %q: 'T' must be followed by time components", s)
		}
	} else {
		datePart = body
	}
	if datePart == "" && timePart == "" {
		return Duration{}, fmt.Errorf("invalid ISO 8601 duration %q: no components", s)
	}

	var d Duration
	if datePart != "" {
		units := map[byte]*int{'Y': &d.years, 'M': &d.months, 'W': &d.weeks, 'D': &d.days}
		if err := parseComponents(s, datePart, "YMWCD", units); err != nil {
			return Duration{}, err
		}
	}
	if timePart != "" {
		units := map[byte]*int{'H': &d.hours, 'M': &d.minutes, 'S': &d.seconds}
		if err := parseComponents(s, timePart, "HMS", units); err != nil {
			return Duration{}, err
		}
	}
	if d.IsZero() {
		return Duration{}, fmt.Errorf("invalid ISO 8601 duration %q: all components are zero", s)
	}
	return d, nil
}

// parseComponents consumes runs of digits followed by one of the units in
// allowed (in that order), each at most once. units maps a unit letter to
// the Duration field that collects its value.
func parseComponents(orig, part, allowed string, units map[byte]*int) error {
	seen := map[byte]bool{}
	for len(part) > 0 {
		n := 0
		for n < len(part) && part[n] >= '0' && part[n] <= '9' {
			n++
		}
		if n == 0 {
			return fmt.Errorf("invalid ISO 8601 duration %q: expected a number before %q", orig, part[:1])
		}
		if n == len(part) {
			return fmt.Errorf("invalid ISO 8601 duration %q: number %q is not followed by a unit", orig, part)
		}
		value, err := strconv.Atoi(part[:n])
		if err != nil {
			return fmt.Errorf("invalid ISO 8601 duration %q: %v", orig, err)
		}
		unit := part[n]
		if !strings.ContainsRune(allowed, rune(unit)) {
			return fmt.Errorf("invalid ISO 8601 duration %q: unexpected unit %q", orig, string(unit))
		}
		if seen[unit] {
			return fmt.Errorf("invalid ISO 8601 duration %q: unit %q appears more than once", orig, string(unit))
		}
		seen[unit] = true
		*units[unit] = value
		part = part[n+1:]
	}
	return nil
}

// IsZero reports whether the duration has no components or all are zero.
func (d Duration) IsZero() bool {
	return d == Duration{}
}

// FixedSeconds returns the duration's length in seconds when it is composed
// only of fixed units (weeks down to seconds), and false when it contains
// calendar units (years, months), whose length depends on the boundary they
// are added to.
func (d Duration) FixedSeconds() (int64, bool) {
	if d.years != 0 || d.months != 0 {
		return 0, false
	}
	return int64(d.weeks)*7*86400 +
		int64(d.days)*86400 +
		int64(d.hours)*3600 +
		int64(d.minutes)*60 +
		int64(d.seconds), true
}

// AddTo advances t by the duration. Calendar units are applied first (via
// AddDate, so month overflow normalizes the same way time.AddDate does),
// then the fixed units.
func (d Duration) AddTo(t time.Time) time.Time {
	t = t.AddDate(d.years, d.months, d.weeks*7+d.days)
	return t.Add(time.Duration(d.hours)*time.Hour +
		time.Duration(d.minutes)*time.Minute +
		time.Duration(d.seconds)*time.Second)
}

// maxCalendarSteps bounds the boundary-by-boundary advancement for
// calendar-unit durations (years, months), which cannot be jumped
// arithmetically. At one step per month this allows a thousand years of
// catch-up; beyond that the anchor is treated as misconfigured.
const maxCalendarSteps = 12000

// Window is the renewal view of a budget's periods at a point in time.
// Periods are half-open: [CurrentStart, NextEnd) is the period containing
// now, and [PrevStart, CurrentStart) is the most recently completed period.
type Window struct {
	// CurrentStart is the start of the period containing now.
	CurrentStart time.Time
	// NextEnd is the end of the current period: the next renewal boundary.
	// It is always after now.
	NextEnd time.Time
	// PrevStart is the start of the most recently completed period. When no
	// period has completed yet (the anchor lies in the future, or now is
	// within the first period), it equals CurrentStart, making the
	// completed window empty.
	PrevStart time.Time
}

// WindowAt locates the period window for a budget anchored at anchor with
// duration d, at time now. When the anchor is far in the past (a pod outage
// or an idle budget skipped several boundaries), the window advances
// directly to the current period in one call — missed periods are not
// replayed individually; only the most recent completed one feeds the
// renewal's overage and rollover computation.
func WindowAt(anchor time.Time, d Duration, now time.Time) (Window, error) {
	current := anchor
	prev := anchor

	if !anchor.After(now) {
		// Fast path for fixed-unit durations: jump most of the way with
		// arithmetic instead of stepping boundary by boundary. The loop
		// below then handles the sub-period remainder (and the calendar-
		// unit case, which cannot be jumped).
		if secs, ok := d.FixedSeconds(); ok && secs > 0 {
			step := time.Duration(secs) * time.Second
			if elapsed := now.Sub(anchor); elapsed >= step {
				steps := int64(elapsed / step)
				current = anchor.Add(time.Duration(steps) * step)
				prev = current.Add(-step)
			}
		}
	}

	steps := 0
	for next := d.AddTo(current); !next.After(now); next = d.AddTo(current) {
		prev = current
		current = next
		steps++
		if _, fixed := d.FixedSeconds(); !fixed && steps > maxCalendarSteps {
			return Window{}, fmt.Errorf("period duration from anchor %s requires more than %d steps to reach now",
				anchor.Format(time.RFC3339), maxCalendarSteps)
		}
	}
	return Window{CurrentStart: current, NextEnd: d.AddTo(current), PrevStart: prev}, nil
}
