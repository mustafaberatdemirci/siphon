package main

// Download hours (Settings → Download hours): outside them the engine holds
// the queue (queue.Engine.SetOffHours) and the status line says until when.
// The hours may wrap past midnight: 23:00–07:00.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

const (
	prefHoursOn   = "hours_on"
	prefHoursFrom = "hours_from"
	prefHoursTo   = "hours_to"

	defaultHoursFrom = "02:00"
	defaultHoursTo   = "08:00"
)

// hoursEvery is how often the clock is looked at.
const hoursEvery = 15 * time.Second

// parseClock reads "HH:MM" as minutes after midnight.
func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(m) != 2 {
		return 0, fmt.Errorf("%s", Tf("Not a time of day: %q (like 02:00)", s))
	}
	return hh*60 + mm, nil
}

// inHours reports whether now is within from–to (minutes after midnight);
// a span past midnight wraps, and from == to is the whole day.
func inHours(now time.Time, from, to int) bool {
	m := now.Hour()*60 + now.Minute()
	switch {
	case from == to:
		return true
	case from < to:
		return m >= from && m < to
	}
	return m >= from || m < to
}

// hoursHold is the hold the settings ask for now: "" when downloads may
// run, else the status line's words.
func hoursHold(prefs fyne.Preferences, now time.Time) string {
	if !prefs.Bool(prefHoursOn) {
		return ""
	}
	fromS := prefs.StringWithFallback(prefHoursFrom, defaultHoursFrom)
	toS := prefs.StringWithFallback(prefHoursTo, defaultHoursTo)
	from, err1 := parseClock(fromS)
	to, err2 := parseClock(toS)
	if err1 != nil || err2 != nil || inHours(now, from, to) {
		return ""
	}
	return Tf("Outside the download hours (%s–%s): downloads start at %s.", fromS, toS, fromS)
}

// applyHours holds or releases the queue for the hours. UI thread.
func (q *queueTab) applyHours(now time.Time) {
	hold := hoursHold(q.prefs, now)
	q.eng.SetOffHours(hold != "")
	q.vm.SetHold(hold)
}

// hoursLoop looks at the clock now and then.
func (q *queueTab) hoursLoop() {
	t := time.NewTicker(hoursEvery)
	defer t.Stop()
	for now := range t.C {
		fyne.Do(func() { q.applyHours(now) })
	}
}

// hoursSettings is the Settings row: on/off and the two times.
func (q *queueTab) hoursSettings() fyne.CanvasObject {
	from := widget.NewEntry()
	from.SetText(q.prefs.StringWithFallback(prefHoursFrom, defaultHoursFrom))
	to := widget.NewEntry()
	to.SetText(q.prefs.StringWithFallback(prefHoursTo, defaultHoursTo))
	validate := func(s string) error { _, err := parseClock(s); return err }
	from.Validator, to.Validator = validate, validate
	save := func(key string) func(string) {
		return func(s string) {
			if validate(s) == nil {
				q.prefs.SetString(key, strings.TrimSpace(s))
				q.applyHours(time.Now())
			}
		}
	}
	from.OnChanged, to.OnChanged = save(prefHoursFrom), save(prefHoursTo)
	on := widget.NewCheck(T("Only between"), func(b bool) {
		q.prefs.SetBool(prefHoursOn, b)
		q.applyHours(time.Now())
	})
	on.SetChecked(q.prefs.Bool(prefHoursOn))
	return container.NewHBox(on, from, widget.NewLabel("–"), to)
}
