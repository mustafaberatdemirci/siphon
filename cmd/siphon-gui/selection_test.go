package main

import (
	"strings"
	"testing"
)

func selected(s *selection, order []string) string { return strings.Join(s.in(order), ",") }

func TestSelectionClickToggleExtend(t *testing.T) {
	order := []string{"a", "b", "c", "d", "e"}
	s := newSelection()

	s.click("b")
	if got := selected(s, order); got != "b" {
		t.Fatalf("click: %s", got)
	}
	// Shift-click selects the range from the last click, either direction.
	s.extend(order, "d")
	if got := selected(s, order); got != "b,c,d" {
		t.Fatalf("shift-click down: %s", got)
	}
	s.extend(order, "a")
	if got := selected(s, order); got != "a,b" {
		t.Fatalf("shift-click up from the same anchor: %s", got)
	}
	// Ctrl-click adds and removes one without touching the rest.
	s.toggle("e")
	if got := selected(s, order); got != "a,b,e" {
		t.Fatalf("ctrl-click add: %s", got)
	}
	s.toggle("a")
	if got := selected(s, order); got != "b,e" {
		t.Fatalf("ctrl-click remove: %s", got)
	}
	// The anchor moved to the last Ctrl-click.
	s.extend(order, "c")
	if got := selected(s, order); got != "a,b,c" {
		t.Fatalf("shift-click after ctrl-click: %s", got)
	}
	s.click("d")
	if got := selected(s, order); got != "d" {
		t.Fatalf("a plain click starts over: %s", got)
	}
}

func TestSelectionFollowsWhatIsShown(t *testing.T) {
	s := newSelection()
	s.all([]string{"a", "b", "c"})
	// b was filtered out or removed: it must not stay selected, or an action
	// would reach a job the user can't see.
	s.keepOnly([]string{"a", "c"})
	if got := selected(s, []string{"a", "b", "c"}); got != "a,c" {
		t.Fatalf("keepOnly: %s", got)
	}
	// Shift-click with the anchor gone is a plain click.
	s.click("b")
	s.keepOnly([]string{"a", "c"})
	s.extend([]string{"a", "c"}, "c")
	if got := selected(s, []string{"a", "c"}); got != "c" {
		t.Fatalf("extend without an anchor: %s", got)
	}
	s.clear()
	if s.has("c") {
		t.Fatal("clear left a selection")
	}
}
