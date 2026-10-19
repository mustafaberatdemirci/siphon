package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseClock(t *testing.T) {
	for s, want := range map[string]int{"02:00": 120, "23:59": 1439, " 7:05 ": 425, "00:00": 0} {
		if got, err := parseClock(s); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", s, got, err, want)
		}
	}
	for _, bad := range []string{"24:00", "7", "07:5", "ab:cd", "12:60", ""} {
		if _, err := parseClock(bad); err == nil {
			t.Errorf("%q was taken for a time", bad)
		}
	}
}

func TestInHours(t *testing.T) {
	at := func(hm string) time.Time {
		m, _ := parseClock(hm)
		return time.Date(2026, 10, 20, m/60, m%60, 0, 0, time.Local)
	}
	for _, tc := range []struct {
		from, to, now string
		want          bool
	}{
		{"02:00", "08:00", "03:00", true},
		{"02:00", "08:00", "08:00", false}, // the end is out
		{"02:00", "08:00", "01:59", false},
		{"23:00", "07:00", "23:30", true}, // past midnight
		{"23:00", "07:00", "06:59", true},
		{"23:00", "07:00", "12:00", false},
		{"05:00", "05:00", "17:00", true}, // the whole day
	} {
		from, _ := parseClock(tc.from)
		to, _ := parseClock(tc.to)
		if got := inHours(at(tc.now), from, to); got != tc.want {
			t.Errorf("%s–%s at %s: %v, want %v", tc.from, tc.to, tc.now, got, tc.want)
		}
	}
}

// Outside the hours the engine holds the queue and the status line says
// until when; turning the setting off lets go.
func TestApplyHours(t *testing.T) {
	q, eng, _ := newTestQueue(t, "a.bin")
	q.prefs.SetBool(prefHoursOn, true)
	q.prefs.SetString(prefHoursFrom, "02:00")
	q.prefs.SetString(prefHoursTo, "08:00")
	noon := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)

	q.applyHours(noon)
	if !eng.OffHours() {
		t.Error("the queue isn't held at noon for 02:00–08:00")
	}
	if got := q.vm.StatusLine(); !strings.Contains(got, "02:00–08:00") {
		t.Errorf("status line: %q", got)
	}
	q.applyHours(noon.Add(-8 * time.Hour)) // 04:00
	if eng.OffHours() || strings.Contains(q.vm.StatusLine(), "Outside") {
		t.Error("still held within the hours")
	}
	q.applyHours(noon)
	q.prefs.SetBool(prefHoursOn, false)
	q.applyHours(noon)
	if eng.OffHours() {
		t.Error("held with the setting off")
	}
}
