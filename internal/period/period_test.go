package period

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) Duration {
	t.Helper()
	d, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return d
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParse_Valid(t *testing.T) {
	cases := []struct {
		in   string
		want Duration
	}{
		{"P1M", Duration{months: 1}},
		{"P3M", Duration{months: 3}},
		{"P7D", Duration{days: 7}},
		{"P1Y", Duration{years: 1}},
		{"P2W", Duration{weeks: 2}},
		{"P1Y6M", Duration{years: 1, months: 6}},
		{"PT1H", Duration{hours: 1}},
		{"PT30M", Duration{minutes: 30}},
		{"PT90S", Duration{seconds: 90}},
		{"P1DT12H", Duration{days: 1, hours: 12}},
		{"P1Y2M3W4DT5H6M7S", Duration{years: 1, months: 2, weeks: 3, days: 4, hours: 5, minutes: 6, seconds: 7}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q): expected %+v, got %+v", c.in, c.want, got)
		}
	}
}

func TestParse_Invalid(t *testing.T) {
	for _, s := range []string{
		"", "P", "P0D", "PT", "1M", "PX", "P1X", "P1", "P1M1M", "PT1H1H",
		"P1.5D", "P-1D", "P1MT", "P1S", "T1H", "P1Mx",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q): expected an error", s)
		}
	}
}

func TestAddTo_CalendarNormalization(t *testing.T) {
	// AddDate semantics: Jan 31 + P1M lands on Mar 3 in non-leap years
	// (Feb 31 normalizes). The store and renewal share this rule, so the
	// test pins it.
	d := mustParse(t, "P1M")
	got := d.AddTo(ts("2027-01-31T00:00:00Z"))
	if want := ts("2027-03-03T00:00:00Z"); !got.Equal(want) {
		t.Errorf("Jan 31 2027 + P1M: expected %s, got %s", want, got)
	}

	got = d.AddTo(ts("2027-01-15T00:00:00Z"))
	if want := ts("2027-02-15T00:00:00Z"); !got.Equal(want) {
		t.Errorf("Jan 15 2027 + P1M: expected %s, got %s", want, got)
	}

	// Fixed units are exact.
	d = mustParse(t, "P7D")
	got = d.AddTo(ts("2026-11-01T00:00:00Z"))
	if want := ts("2026-11-08T00:00:00Z"); !got.Equal(want) {
		t.Errorf("Nov 1 + P7D: expected %s, got %s", want, got)
	}
}

func TestWindowAt_FirstPeriod(t *testing.T) {
	anchor := ts("2026-11-01T00:00:00Z")
	d := mustParse(t, "P1M")
	now := ts("2026-11-15T00:00:00Z")

	w, err := WindowAt(anchor, d, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !w.CurrentStart.Equal(anchor) {
		t.Errorf("CurrentStart: expected %s, got %s", anchor, w.CurrentStart)
	}
	if !w.NextEnd.Equal(ts("2026-12-01T00:00:00Z")) {
		t.Errorf("NextEnd: expected 2026-12-01, got %s", w.NextEnd)
	}
	// No period has completed: the previous window is empty.
	if !w.PrevStart.Equal(w.CurrentStart) {
		t.Errorf("PrevStart: expected %s (empty window), got %s", w.CurrentStart, w.PrevStart)
	}
}

func TestWindowAt_FutureAnchor(t *testing.T) {
	anchor := ts("2026-12-01T00:00:00Z")
	d := mustParse(t, "P1M")
	now := ts("2026-11-15T00:00:00Z")

	w, err := WindowAt(anchor, d, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !w.CurrentStart.Equal(anchor) || !w.NextEnd.Equal(ts("2027-01-01T00:00:00Z")) {
		t.Errorf("future anchor: expected [2026-12-01, 2027-01-01), got [%s, %s)", w.CurrentStart, w.NextEnd)
	}
}

func TestWindowAt_BoundaryExactlyAtNow(t *testing.T) {
	// A boundary at exactly `now` closes the period ending there: the
	// current period starts at the boundary.
	anchor := ts("2026-11-01T00:00:00Z")
	d := mustParse(t, "P7D")
	now := ts("2026-11-08T00:00:00Z")

	w, err := WindowAt(anchor, d, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !w.CurrentStart.Equal(now) {
		t.Errorf("CurrentStart: expected %s, got %s", now, w.CurrentStart)
	}
	if !w.PrevStart.Equal(anchor) {
		t.Errorf("PrevStart: expected %s, got %s", anchor, w.PrevStart)
	}
}

func TestWindowAt_CatchUpSkipsMissedPeriods(t *testing.T) {
	// The anchor is months in the past and the current period is the 4th
	// one: the window must jump straight to it, with the previous period
	// (not every missed one) as the completed window.
	anchor := ts("2026-08-01T00:00:00Z")
	d := mustParse(t, "P1M")
	now := ts("2026-11-15T00:00:00Z")

	w, err := WindowAt(anchor, d, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !w.CurrentStart.Equal(ts("2026-11-01T00:00:00Z")) {
		t.Errorf("CurrentStart: expected 2026-11-01, got %s", w.CurrentStart)
	}
	if !w.NextEnd.Equal(ts("2026-12-01T00:00:00Z")) {
		t.Errorf("NextEnd: expected 2026-12-01, got %s", w.NextEnd)
	}
	if !w.PrevStart.Equal(ts("2026-10-01T00:00:00Z")) {
		t.Errorf("PrevStart: expected 2026-10-01, got %s", w.PrevStart)
	}
}

func TestWindowAt_FixedDurationFastPath(t *testing.T) {
	// A seconds-scale duration with a far anchor exercises the arithmetic
	// jump; the result must match plain stepping.
	anchor := ts("2026-11-01T00:00:00Z")
	d := mustParse(t, "PT5S")
	now := ts("2026-11-01T00:02:13Z") // 133s → 26 full periods, 3s into the 27th

	w, err := WindowAt(anchor, d, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := anchor.Add(26 * 5 * time.Second); !w.CurrentStart.Equal(want) {
		t.Errorf("CurrentStart: expected %s, got %s", want, w.CurrentStart)
	}
	if want := anchor.Add(27 * 5 * time.Second); !w.NextEnd.Equal(want) {
		t.Errorf("NextEnd: expected %s, got %s", want, w.NextEnd)
	}
	if want := anchor.Add(25 * 5 * time.Second); !w.PrevStart.Equal(want) {
		t.Errorf("PrevStart: expected %s, got %s", want, w.PrevStart)
	}
}

func TestWindowAt_FixedDurationFastPathExactBoundary(t *testing.T) {
	// now exactly on a boundary after a multi-period jump: the completed
	// window is the period ending at now, not an empty one.
	anchor := ts("2026-11-01T00:00:00Z")
	d := mustParse(t, "PT10S")
	now := anchor.Add(3 * 10 * time.Second)

	w, err := WindowAt(anchor, d, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !w.CurrentStart.Equal(now) {
		t.Errorf("CurrentStart: expected %s, got %s", now, w.CurrentStart)
	}
	if want := anchor.Add(2 * 10 * time.Second); !w.PrevStart.Equal(want) {
		t.Errorf("PrevStart: expected %s, got %s", want, w.PrevStart)
	}
}

func TestFixedSeconds(t *testing.T) {
	if _, ok := mustParse(t, "P1M").FixedSeconds(); ok {
		t.Error("P1M must not be fixed")
	}
	if _, ok := mustParse(t, "P1Y").FixedSeconds(); ok {
		t.Error("P1Y must not be fixed")
	}
	secs, ok := mustParse(t, "P1WT2H3M4S").FixedSeconds()
	if !ok {
		t.Fatal("P1WT2H3M4S must be fixed")
	}
	if want := int64(7*86400 + 2*3600 + 3*60 + 4); secs != want {
		t.Errorf("expected %d seconds, got %d", want, secs)
	}
}
