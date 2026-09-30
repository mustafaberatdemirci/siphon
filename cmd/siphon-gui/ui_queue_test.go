package main

import (
	"context"
	"image/color"
	"math"
	"reflect"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// listResolver hands out a fixed list of files for https://list.test/…
type listResolver struct{ names []string }

func (r listResolver) Match(u string) bool { return strings.HasPrefix(u, "https://list.test/") }
func (r listResolver) Resolve(_ context.Context, u string, yield func(site.Item) error) ([]site.ItemError, error) {
	for i, n := range r.names {
		it := site.Item{URL: u + "/" + n, SourcePage: u + "#" + n, Filename: n, Size: 100, Index: i}
		if err := yield(it); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
func (listResolver) ResolveOne(context.Context, string) (site.Item, error) { return site.Item{}, nil }
func (listResolver) Diagnose(context.Context) ([]site.LayerResult, error)  { return nil, nil }

// newTestQueue is the Download tab in a test window over a real engine that
// never runs: added jobs stay queued.
func newTestQueue(t *testing.T, names ...string) (*queueTab, *queue.Engine, fyne.Window) {
	t.Helper()
	autoRefresh = false
	t.Cleanup(func() { autoRefresh = true })
	a := test.NewTempApp(t)
	vm := newViewModel()
	eng, err := queue.New(queue.Options{
		Resolvers: []site.Resolver{listResolver{names: names}},
		Configs:   []site.SiteConfig{site.SiteConfig{Name: "list"}.WithDefaults()},
		OnChange:  vm.Apply,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := test.NewTempWindow(t, container.NewStack())
	q, view := newQueueTab(w, a.Preferences(), eng, vm, nil)
	w.SetContent(view)
	w.Resize(fyne.NewSize(1000, 700))
	if _, err := eng.Add(context.Background(), "https://list.test/album", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	vm.Replace(eng.Jobs())
	q.render()
	return q, eng, w
}

func (q *queueTab) idOf(t *testing.T, name string) string {
	t.Helper()
	for _, r := range q.visible {
		if r.Job.Filename == name {
			return r.Job.ID
		}
	}
	t.Fatalf("%s is not shown", name)
	return ""
}

func stateOf(eng *queue.Engine, id string) queue.State {
	for _, j := range eng.Jobs() {
		if j.ID == id {
			return j.State
		}
	}
	return ""
}

func TestTableSelectionDrivesTheToolbar(t *testing.T) {
	q, eng, _ := newTestQueue(t, "a.bin", "b.bin", "c.bin")
	if len(q.visible) != 3 {
		t.Fatalf("%d rows shown", len(q.visible))
	}
	if !q.resumeSel.Disabled() || !q.pauseSel.Disabled() || !q.removeSel.Disabled() {
		t.Fatal("selection buttons enabled with nothing selected")
	}

	a := q.idOf(t, "a.bin")
	q.rowPressed(a, desktop.MouseButtonPrimary, 0)
	if q.pauseSel.Disabled() || !q.resumeSel.Disabled() || q.removeSel.Disabled() {
		t.Fatal("a queued job selected: Pause and Remove should be on, Resume off")
	}
	q.pauseSelected()
	if st := stateOf(eng, a); st != queue.StatePaused {
		t.Fatalf("Pause left the job %s", st)
	}
	if q.resumeSel.Disabled() || !q.pauseSel.Disabled() {
		t.Fatal("a paused job selected: Resume should be on, Pause off")
	}

	// Shift-click takes the range; the menu counts what each action reaches.
	q.rowPressed(q.idOf(t, "c.bin"), desktop.MouseButtonPrimary, fyne.KeyModifierShift)
	labels := map[string]bool{}
	for _, it := range q.rowMenu().Items {
		if !it.IsSeparator {
			labels[it.Label] = !it.Disabled
		}
	}
	for label, on := range map[string]bool{"Resume": true, "Pause (2)": true, "Show in folder": false, "Show error": false, "Copy link (3)": true, "Remove (3)": true} {
		if got, ok := labels[label]; !ok || got != on {
			t.Errorf("menu item %q: present %v, enabled %v; want enabled %v (menu: %v)", label, ok, got, on, labels)
		}
	}

	// A right-click on a row outside the selection selects that row alone.
	q.rowPressed(a, desktop.MouseButtonPrimary, 0)
	q.rowPressed(q.idOf(t, "b.bin"), desktop.MouseButtonSecondary, 0)
	if got := len(q.selectedJobs()); got != 1 || q.selectedJobs()[0].Filename != "b.bin" {
		t.Errorf("right-click selected %d jobs", got)
	}
}

func TestTableFilterKeepsSelectionToWhatIsShown(t *testing.T) {
	q, eng, _ := newTestQueue(t, "a.bin", "b.bin", "c.bin")
	a := q.idOf(t, "a.bin")
	eng.Pause(a)
	q.vm.Replace(eng.Jobs())
	q.render()

	q.sel.all(q.visibleIDs)
	paused := stateNodeID(filterPaused)
	if n := q.counts[paused]; n != 1 {
		t.Errorf("Paused counts %d", n)
	}
	q.tree.Select(paused)
	if len(q.visible) != 1 || q.visible[0].Job.ID != a {
		t.Fatalf("Paused shows %d rows", len(q.visible))
	}
	// Removing now must reach only the paused job, not the hidden ones.
	q.removeSelected()
	if n := len(eng.Jobs()); n != 2 {
		t.Fatalf("%d jobs left, want 2", n)
	}
}

func TestDeleteKeyOnlyOnTheDownloadTab(t *testing.T) {
	q, eng, w := newTestQueue(t, "a.bin", "b.bin")
	q.sel.all(q.visibleIDs)
	onTab := false
	q.shown = func() bool { return onTab }
	press := w.Canvas().OnTypedKey()

	press(&fyne.KeyEvent{Name: fyne.KeyDelete})
	if n := len(eng.Jobs()); n != 2 {
		t.Fatalf("Delete on another tab removed jobs: %d left", n)
	}
	onTab = true
	press(&fyne.KeyEvent{Name: fyne.KeyDelete})
	if n := len(eng.Jobs()); n != 0 {
		t.Fatalf("Delete on the table left %d jobs", n)
	}
}

func TestCopyLinks(t *testing.T) {
	q, _, _ := newTestQueue(t, "a.bin", "b.bin")
	q.sel.all(q.visibleIDs)
	q.copySelectedLinks()
	got := fyne.CurrentApp().Clipboard().Content()
	if got != "https://list.test/album#a.bin\nhttps://list.test/album#b.bin" {
		t.Errorf("clipboard = %q", got)
	}
}

func TestHeaderSorts(t *testing.T) {
	q, _, _ := newTestQueue(t, "b.bin", "a.bin", "c.bin")
	q.sortBy(sortName)
	if got := ids(q.visible); got != strings.Join([]string{q.idOf(t, "a.bin"), q.idOf(t, "b.bin"), q.idOf(t, "c.bin")}, ",") {
		t.Errorf("sorted by name: %s", got)
	}
	q.sortBy(sortName)
	if q.visible[0].Job.Filename != "c.bin" {
		t.Errorf("descending starts with %s", q.visible[0].Job.Filename)
	}
	q.sortBy(sortName)
	if q.visible[0].Job.Filename != "b.bin" {
		t.Errorf("back to queue order starts with %s", q.visible[0].Job.Filename)
	}
}

// typeShortcut fires sc through the window's canvas the way the desktop
// driver hands Ctrl+A and Ctrl+V over: as ShortcutSelectAll and
// ShortcutPaste, not as key combinations. (The test window wraps the canvas
// that dispatches shortcuts; it is reached through the embedded field.)
func typeShortcut(t *testing.T, w fyne.Window, sc fyne.Shortcut) {
	t.Helper()
	inner := reflect.ValueOf(w.Canvas()).Elem().FieldByName("WindowlessCanvas")
	h, ok := inner.Interface().(interface{ TypedShortcut(fyne.Shortcut) })
	if !ok {
		t.Fatal("the test canvas no longer dispatches shortcuts this way")
	}
	h.TypedShortcut(sc)
}

func TestCtrlASelectsEveryRowShown(t *testing.T) {
	q, _, w := newTestQueue(t, "a.bin", "b.bin", "c.bin")
	typeShortcut(t, w, &fyne.ShortcutSelectAll{})
	if n := len(q.selectedJobs()); n != 3 {
		t.Fatalf("Ctrl+A selected %d rows, want 3", n)
	}
}

func TestCtrlVOpensAddWithThePastedLinks(t *testing.T) {
	q, _, w := newTestQueue(t, "a.bin")
	q.pending = "https://list.test/failed-before"
	fyne.CurrentApp().Clipboard().SetContent("https://list.test/pasted\n")
	typeShortcut(t, w, &fyne.ShortcutPaste{Clipboard: fyne.CurrentApp().Clipboard()})
	if q.addEntry == nil {
		t.Fatal("Ctrl+V didn't open the Add dialog")
	}
	if got := q.addEntry.Text; got != "https://list.test/failed-before\nhttps://list.test/pasted" {
		t.Errorf("the dialog opened with %q", got)
	}
}

// With a dialog open over the table, Delete and Ctrl+A don't reach the
// rows behind it.
func TestKeysLeaveTheTableAloneUnderADialog(t *testing.T) {
	q, eng, w := newTestQueue(t, "a.bin", "b.bin")
	q.sel.all(q.visibleIDs)
	q.showSettings()
	w.Canvas().Unfocus()
	w.Canvas().OnTypedKey()(&fyne.KeyEvent{Name: fyne.KeyDelete})
	if n := len(eng.Jobs()); n != 2 {
		t.Fatalf("Delete under the Settings dialog removed jobs: %d left", n)
	}
}

// The menu bar holds what the toolbar leaves out; its Quit is Siphon's own
// (saving unfinished jobs), so Fyne doesn't add one that wouldn't.
func TestMainMenu(t *testing.T) {
	q, _, _ := newTestQueue(t, "a.bin")
	labels := func(m *fyne.MainMenu) map[string]*fyne.MenuItem {
		out := map[string]*fyne.MenuItem{}
		for _, menu := range m.Items {
			for _, it := range menu.Items {
				if !it.IsSeparator {
					out[menu.Label+" > "+it.Label] = it
				}
			}
		}
		return out
	}
	quit := false
	got := labels(q.mainMenu(func() { quit = true }))
	for _, want := range []string{"Tasks > Add links…", "Tasks > Settings…", "Tasks > Quit",
		"Downloads > Retry failed", "Downloads > Clear finished", "Downloads > Cancel all…",
		"Help > Diagnose…", "Help > Install tools…", "Help > About Siphon"} {
		if got[want] == nil {
			t.Errorf("%q missing from the menu bar", want)
		}
	}
	if it := got["Tasks > Quit"]; it == nil || !it.IsQuit {
		t.Fatal("Quit isn't marked as the app's quit item")
	} else {
		it.Action()
	}
	if !quit {
		t.Error("Quit didn't call Siphon's quit")
	}
	if labels(q.mainMenu(nil))["Tasks > Quit"] != nil {
		t.Error("a Quit without a quit function")
	}
}

// Links that couldn't be added wait for the next Add dialog; the ones
// that worked are queued, and the folder becomes the default.
func TestResolveLinksKeepsTheFailedOnes(t *testing.T) {
	q, eng, _ := newTestQueue(t, "a.bin")
	dir := t.TempDir()
	added, errs, failed := q.resolveLinks([]string{"https://list.test/other", "https://unknown.test/x"}, dir)
	if added != 1 || len(errs) != 1 || len(failed) != 1 || failed[0] != "https://unknown.test/x" {
		t.Errorf("added %d, errs %v, failed %v", added, errs, failed)
	}
	if n := len(eng.Jobs()); n != 2 {
		t.Errorf("%d jobs, want the first album's and the new one", n)
	}

	// Without a folder nothing is resolved and the links are kept.
	q.addLinks("https://list.test/kept", "   ")
	if q.pending != "https://list.test/kept" {
		t.Errorf("pending = %q", q.pending)
	}
}

// The category tree's counts are drawn readable: the item shows the number,
// and on both variants its colour stands out from the background at least
// 4.5:1 (it was the disabled colour, about 1.6:1 on the dark theme).
func TestTreeCountsAreReadable(t *testing.T) {
	test.NewTempApp(t)
	o := newTreeItem(false)
	setTreeItem(o, "All downloads", 520)
	count := o.(*fyne.Container).Objects[1].(*container.ThemeOverride).Content.(*widget.Label)
	if count.Text != "520" {
		t.Fatalf("count shows %q", count.Text)
	}
	setTreeItem(o, "Done", 0)
	if count.Text != "" {
		t.Errorf("an empty category shows %q", count.Text)
	}
	th := countTheme{newCompactTheme()}
	for _, v := range []fyne.ThemeVariant{theme.VariantDark, theme.VariantLight} {
		bg := th.Color(theme.ColorNameBackground, v)
		fg := over(th.Color(theme.ColorNameDisabled, v), bg)
		if c := contrast(fg, bg); c < 4.5 {
			t.Errorf("variant %d: count contrast %.1f:1, want at least 4.5:1", v, c)
		}
	}
}

// over paints a translucent colour onto an opaque background.
func over(c, bg color.Color) color.Color {
	f := color.NRGBAModel.Convert(c).(color.NRGBA)
	b := color.NRGBAModel.Convert(bg).(color.NRGBA)
	a := float64(f.A) / 0xff
	mix := func(x, y uint8) uint8 { return uint8(a*float64(x) + (1-a)*float64(y) + 0.5) }
	return color.NRGBA{R: mix(f.R, b.R), G: mix(f.G, b.G), B: mix(f.B, b.B), A: 0xff}
}

// contrast is the WCAG contrast ratio of two opaque colours.
func contrast(a, b color.Color) float64 {
	lum := func(c color.Color) float64 {
		r, g, b, _ := c.RGBA()
		ch := func(v uint32) float64 {
			s := float64(v) / 0xffff
			if s <= 0.03928 {
				return s / 12.92
			}
			return math.Pow((s+0.055)/1.055, 2.4)
		}
		return 0.2126*ch(r) + 0.7152*ch(g) + 0.0722*ch(b)
	}
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
