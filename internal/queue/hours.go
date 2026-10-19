package queue

// Download hours: outside the hours the user set (a night tariff, a quiet
// working day), nothing starts and the running downloads stop; they stay
// queued, not paused, and go on by themselves when the hours come. Apart
// from the user's own Pause all, which stays as it is. The window decides
// when the hours are (it has the clock and the settings) and says so here.

// SetOffHours turns the hold on (outside the download hours) or off.
func (e *Engine) SetOffHours(off bool) {
	e.mu.Lock()
	if e.offHours == off {
		e.mu.Unlock()
		return
	}
	e.offHours = off
	var stop []func()
	if off {
		for _, cancel := range e.active {
			stop = append(stop, cancel) // not pauseWant: they end up queued
		}
	}
	e.mu.Unlock()
	for _, cancel := range stop {
		cancel()
	}
	if !off {
		e.kick()
	}
}

// OffHours reports whether the hold is on.
func (e *Engine) OffHours() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.offHours
}
