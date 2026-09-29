package main

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// refreshEvery is how often at most the list is redrawn. Drawing per event
// meant hundreds of refreshes per second with 20 parallel downloads; here it
// is drawn if something changed, otherwise not at all.
const refreshEvery = 150 * time.Millisecond

// sidebarWidth is the width of the filter list left of the table.
const sidebarWidth = 190

// autoRefresh starts the loop that redraws on engine events; tests draw by
// hand instead, so the loop doesn't draw alongside them.
var autoRefresh = true

const (
	prefOutDir     = "out_dir"
	prefSpeedLimit = "speed_limit_mbps"
	prefSegments   = "segments"
	prefMaxActive  = "max_active"
	prefQuotaCmd   = "quota_command"
)

// queueTab is the main window's content: a one-line toolbar, the queue
// table with its filters, and the status line. Adding links and the
// settings open in their own dialogs, so the table gets the window.
type queueTab struct {
	win   fyne.Window
	prefs fyne.Preferences
	eng   *queue.Engine
	vm    *viewModel

	// Settings, built once: they apply the saved values at start and are
	// shown in the Settings dialog.
	folder   *widget.Entry
	speed    *widget.Entry
	quotaCmd *widget.Entry
	segments *widget.Select
	settings fyne.CanvasObject

	banner     *fyne.Container
	bannerText *widget.Label
	addBtn     *widget.Button
	resumeSel  *widget.Button
	pauseSel   *widget.Button
	removeSel  *widget.Button
	pauseAll   *widget.Button
	list       *widget.List
	sidebar    *widget.List
	status     *widget.Label
	sortArrows func(sortOrder)

	// pending are links that couldn't be added: the Add dialog opens with
	// them, so the user fixes them instead of copying them again.
	pending string
	// addEntry is the open Add dialog's link box; nil when it is closed.
	addEntry *widget.Entry

	// What the table shows, taken on the UI thread at each render so the
	// list's callbacks read one consistent picture.
	filter     filter
	order      sortOrder
	visible    []row
	visibleIDs []string
	entries    []sidebarEntry
	sel        *selection

	// shown reports whether the table is on screen: the keyboard shortcuts
	// act on it only then. nil means always.
	shown func() bool

	// openDiagnose opens the Diagnose window. May be nil.
	openDiagnose func()

	// onQuota is told, on the UI thread, whether downloads wait for quota
	// (the notification-area icon changes). May be nil.
	onQuota func(waiting bool)
}

func newQueueTab(win fyne.Window, prefs fyne.Preferences, eng *queue.Engine, vm *viewModel, onQuota func(bool)) (*queueTab, fyne.CanvasObject) {
	q := &queueTab{win: win, prefs: prefs, eng: eng, vm: vm, onQuota: onQuota, sel: newSelection()}
	q.settings = q.buildSettings()

	// --- Toolbar ---
	// Add links; what acts on the selected rows (as the row menu does); the
	// queue as a whole; then the rarely used under More. Diagnose and
	// Settings on the right.
	q.addBtn = widget.NewButtonWithIcon("Add links", theme.ContentAddIcon(), func() { q.showAddDialog("") })
	q.addBtn.Importance = widget.HighImportance
	q.resumeSel = toolButton("Resume", theme.MediaPlayIcon(), q.resumeSelected)
	q.pauseSel = toolButton("Pause", theme.MediaPauseIcon(), q.pauseSelected)
	q.removeSel = toolButton("Remove", theme.DeleteIcon(), q.removeSelected)
	q.pauseAll = toolButton("Pause all", theme.MediaPauseIcon(), q.togglePauseAll)
	more := toolButton("More", theme.MoreHorizontalIcon(), nil)
	more.OnTapped = func() {
		widget.ShowPopUpMenuAtRelativePosition(q.moreMenu(), win.Canvas(), fyne.NewPos(0, more.Size().Height), more)
	}
	diagnose := toolButton("Diagnose", theme.InfoIcon(), func() {
		if q.openDiagnose != nil {
			q.openDiagnose()
		}
	})
	settings := toolButton("Settings", theme.SettingsIcon(), q.showSettings)
	toolbar := container.NewBorder(nil, nil,
		container.NewHBox(q.addBtn, widget.NewSeparator(), q.resumeSel, q.pauseSel, q.removeSel,
			widget.NewSeparator(), q.pauseAll, more),
		container.NewHBox(diagnose, settings))

	// --- Quota banner ---
	// Sits above the table while jobs wait for quota; independent of the
	// notification settings, the first thing seen when the window opens.
	// "Try now" is for the user who switched the VPN and doesn't want to wait
	// for the next probe (~10 s).
	q.bannerText = widget.NewLabel("")
	q.bannerText.Wrapping = fyne.TextWrapWord
	q.bannerText.TextStyle = fyne.TextStyle{Bold: true}
	retryBtn := widget.NewButton("Try now", func() {
		eng.RetryWaiting()
		vm.Replace(eng.Jobs())
		q.render()
	})
	retryBtn.Importance = widget.HighImportance
	q.banner = container.NewBorder(nil, nil, widget.NewIcon(theme.WarningIcon()), retryBtn, q.bannerText)
	q.banner.Hide()

	// --- Table ---
	header, arrows := newHeader(q.sortBy)
	q.sortArrows = arrows
	q.list = widget.NewList(
		func() int { return len(q.visible) },
		func() fyne.CanvasObject { return newJobRow(q) },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			if i < 0 || i >= len(q.visible) {
				return
			}
			r := q.visible[i]
			o.(*jobRow).set(r, q.sel.has(r.Job.ID))
		},
	)
	table := container.NewBorder(header, nil, nil, nil, q.list)

	// --- Sidebar: filters with counts ---
	q.sidebar = widget.NewList(
		func() int { return len(q.entries) },
		newSidebarItem,
		func(i widget.ListItemID, o fyne.CanvasObject) {
			if i >= 0 && i < len(q.entries) {
				setSidebarItem(o, q.entries[i])
			}
		},
	)
	q.sidebar.OnSelected = func(i widget.ListItemID) {
		if i < 0 || i >= len(q.entries) || q.entries[i].f == q.filter {
			return
		}
		q.filter = q.entries[i].f
		q.render()
		q.list.ScrollToTop()
	}
	width := canvas.NewRectangle(color.Transparent)
	width.SetMinSize(fyne.NewSize(sidebarWidth, 0))
	sidebar := container.NewStack(width, q.sidebar)

	q.status = widget.NewLabel(vm.StatusLine())
	q.status.Wrapping = fyne.TextWrapWord

	top := container.NewVBox(toolbar, widget.NewSeparator(), q.banner)
	body := container.NewBorder(nil, nil, container.NewHBox(sidebar, widget.NewSeparator()), nil, table)
	bottom := container.NewVBox(widget.NewSeparator(), q.status)
	root := container.NewBorder(top, bottom, nil, nil, body)

	q.bindKeys()
	q.render()
	if autoRefresh {
		go q.refreshLoop()
	}
	return q, root
}

// toolButton is a flat toolbar button.
func toolButton(label string, icon fyne.Resource, tapped func()) *widget.Button {
	b := widget.NewButtonWithIcon(label, icon, tapped)
	b.Importance = widget.LowImportance
	return b
}

// moreMenu holds the queue-wide actions used now and then.
func (q *queueTab) moreMenu() *fyne.Menu {
	c := q.vm.Counts()
	retry := fyne.NewMenuItem("Retry failed", func() {
		q.eng.RetryFailed()
		q.vm.Replace(q.eng.Jobs())
		q.render()
	})
	retry.Icon, retry.Disabled = theme.ViewRefreshIcon(), c[filterFailed] == 0
	clear := fyne.NewMenuItem("Clear finished", func() {
		q.eng.ClearFinished()
		q.vm.Replace(q.eng.Jobs())
		q.render()
	})
	clear.Icon, clear.Disabled = theme.ContentClearIcon(), c[filterFinished] == 0
	cancel := fyne.NewMenuItem("Cancel all…", q.confirmCancelAll)
	cancel.Icon, cancel.Disabled = theme.CancelIcon(), c[filterUnfinished] == 0
	open := fyne.NewMenuItem("Open download folder", q.openFolder)
	open.Icon = theme.FolderOpenIcon()
	return fyne.NewMenu("", retry, clear, cancel, fyne.NewMenuItemSeparator(), open)
}

// onTable reports whether keys typed now are meant for the table: it is on
// screen and no dialog or menu is open over it.
func (q *queueTab) onTable() bool {
	if len(q.win.Canvas().Overlays().List()) > 0 {
		return false
	}
	return q.shown == nil || q.shown()
}

// bindKeys: Delete removes the selected rows, Ctrl+A selects every row
// shown, Ctrl+V opens the Add dialog with the pasted links. A text box with
// the focus keeps its own keys; Fyne hands Ctrl+A and Ctrl+V over as
// ShortcutSelectAll and ShortcutPaste.
func (q *queueTab) bindKeys() {
	c := q.win.Canvas()
	c.SetOnTypedKey(func(ev *fyne.KeyEvent) {
		if ev.Name == fyne.KeyDelete && q.onTable() {
			q.removeSelected()
		}
	})
	c.AddShortcut(&fyne.ShortcutSelectAll{}, func(fyne.Shortcut) {
		if q.onTable() {
			q.sel.all(q.visibleIDs)
			q.renderSelection()
		}
	})
	c.AddShortcut(&fyne.ShortcutPaste{}, func(sc fyne.Shortcut) {
		if !q.onTable() {
			return
		}
		text := ""
		if p, ok := sc.(*fyne.ShortcutPaste); ok && p.Clipboard != nil {
			text = p.Clipboard.Content()
		} else {
			text = fyne.CurrentApp().Clipboard().Content()
		}
		if strings.TrimSpace(text) != "" {
			q.showAddDialog(text)
		}
	})
}

// refreshLoop redraws the table and the status line if the model changed.
func (q *queueTab) refreshLoop() {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for range t.C {
		if !q.vm.TakeDirty() {
			continue
		}
		fyne.Do(q.render)
	}
}

// render takes a fresh picture of the queue and redraws what depends on it.
// UI thread only.
func (q *queueTab) render() {
	q.visible = q.vm.View(q.filter, q.order)
	q.visibleIDs = q.visibleIDs[:0]
	for _, r := range q.visible {
		q.visibleIDs = append(q.visibleIDs, r.Job.ID)
	}
	q.sel.keepOnly(q.visibleIDs)

	q.entries = sidebarEntries(q.vm.Counts(), q.filter)
	q.sidebar.Refresh()
	for i, e := range q.entries {
		if e.f == q.filter {
			q.sidebar.Select(i)
		}
	}

	q.list.Refresh()
	q.updateSelectionButtons()
	q.status.SetText(q.vm.StatusLine())

	banner := q.vm.QuotaBanner()
	if banner == "" {
		q.banner.Hide()
	} else {
		q.bannerText.SetText(banner)
		q.banner.Show()
	}
	if q.onQuota != nil {
		q.onQuota(banner != "")
	}
	switch captcha := q.eng.CaptchaHeld(); {
	case q.eng.Paused():
		q.pauseAll.SetText("Resume all")
		q.pauseAll.SetIcon(theme.MediaPlayIcon())
	case len(captcha) > 0:
		// The reason must be visible: a captcha held that site's jobs (the
		// other sites go on); it resumes when the user resumes one of that
		// site's jobs or presses here.
		q.pauseAll.SetText("Captcha: " + strings.Join(captcha, ", ") + " paused — resume")
		q.pauseAll.SetIcon(theme.MediaPlayIcon())
	default:
		q.pauseAll.SetText("Pause all")
		q.pauseAll.SetIcon(theme.MediaPauseIcon())
	}
}

// renderSelection redraws after the selection alone changed.
func (q *queueTab) renderSelection() {
	q.list.Refresh()
	q.updateSelectionButtons()
}

func (q *queueTab) updateSelectionButtons() {
	a := actionsFor(q.selectedJobs())
	enable(q.resumeSel, a.resume > 0)
	enable(q.pauseSel, a.pause > 0)
	enable(q.removeSel, a.count > 0)
}

func enable(b *widget.Button, on bool) {
	if on {
		b.Enable()
	} else {
		b.Disable()
	}
}

func (q *queueTab) sortBy(k sortKey) {
	q.order = q.order.next(k)
	q.sortArrows(q.order)
	q.render()
}

// selectedJobs are the selected rows' jobs, in the order shown.
func (q *queueTab) selectedJobs() []queue.Job {
	var out []queue.Job
	for _, r := range q.visible {
		if q.sel.has(r.Job.ID) {
			out = append(out, r.Job)
		}
	}
	return out
}

// rowPressed is a mouse button going down on a row.
func (q *queueTab) rowPressed(id string, b desktop.MouseButton, mod fyne.KeyModifier) {
	switch {
	case b == desktop.MouseButtonSecondary:
		if !q.sel.has(id) {
			q.sel.click(id)
		}
	case mod&fyne.KeyModifierShift != 0:
		q.sel.extend(q.visibleIDs, id)
	case mod&(fyne.KeyModifierControl|fyne.KeyModifierSuper) != 0:
		q.sel.toggle(id)
	default:
		q.sel.click(id)
	}
	q.renderSelection()
}

// rowDoubleTapped shows a finished file in its folder, or why a job
// failed. Opening the file itself is left to the user: it may be a program.
func (q *queueTab) rowDoubleTapped(id string) {
	for _, r := range q.visible {
		switch {
		case r.Job.ID != id:
		case r.Job.State.Finished():
			q.showInFolder(r.Job)
		case r.Job.Error != "":
			q.showError(r.Job)
		}
	}
}

// showError shows a job's whole error, which the Status column cuts short,
// with the link it came from.
func (q *queueTab) showError(j queue.Job) {
	msg := widget.NewLabel(j.Error)
	msg.Wrapping = fyne.TextWrapWord
	content := container.NewVBox(msg)
	if links := sourceLinks([]queue.Job{j}); len(links) > 0 {
		from := widget.NewLabel("From: " + links[0])
		from.Wrapping = fyne.TextWrapBreak
		from.Importance = widget.LowImportance
		content.Add(from)
	}
	d := dialog.NewCustom(j.Filename, "Close", content, q.win)
	d.Resize(fyne.NewSize(560, 0))
	d.Show()
}

// showRowMenu opens the actions for the selected rows at pos.
func (q *queueTab) showRowMenu(pos fyne.Position) {
	if menu := q.rowMenu(); menu != nil {
		widget.ShowPopUpMenuAtPosition(menu, q.win.Canvas(), pos)
	}
}

// rowMenu is the right-click menu for the selected rows; nil without a
// selection.
func (q *queueTab) rowMenu() *fyne.Menu {
	jobs := q.selectedJobs()
	if len(jobs) == 0 {
		return nil
	}
	a := actionsFor(jobs)
	resume := fyne.NewMenuItem(counted("Resume", a.resume), q.resumeSelected)
	resume.Icon, resume.Disabled = theme.MediaPlayIcon(), a.resume == 0
	pause := fyne.NewMenuItem(counted("Pause", a.pause), q.pauseSelected)
	pause.Icon, pause.Disabled = theme.MediaPauseIcon(), a.pause == 0
	show := fyne.NewMenuItem("Show in folder", func() { q.showInFolder(jobs[0]) })
	show.Icon, show.Disabled = theme.FolderOpenIcon(), len(jobs) != 1
	showErr := fyne.NewMenuItem("Show error", func() { q.showError(jobs[0]) })
	showErr.Icon, showErr.Disabled = theme.ErrorIcon(), len(jobs) != 1 || jobs[0].Error == ""
	copyLinks := fyne.NewMenuItem(counted("Copy link", len(jobs)), q.copySelectedLinks)
	copyLinks.Icon = theme.ContentCopyIcon()
	remove := fyne.NewMenuItem(counted("Remove", len(jobs)), q.removeSelected)
	remove.Icon = theme.DeleteIcon()
	selectAll := fyne.NewMenuItem("Select all", func() {
		q.sel.all(q.visibleIDs)
		q.renderSelection()
	})
	return fyne.NewMenu("", resume, pause, fyne.NewMenuItemSeparator(),
		show, showErr, copyLinks, fyne.NewMenuItemSeparator(), remove, fyne.NewMenuItemSeparator(), selectAll)
}

func (q *queueTab) resumeSelected() {
	for _, j := range q.selectedJobs() {
		if actionFor(j.State) == actionResume {
			q.eng.Resume(j.ID)
		}
	}
	q.vm.Replace(q.eng.Jobs())
	q.render()
}

func (q *queueTab) pauseSelected() {
	for _, j := range q.selectedJobs() {
		if actionFor(j.State) == actionPause {
			q.eng.Pause(j.ID)
		}
	}
	q.vm.Replace(q.eng.Jobs())
	q.render()
}

// copySelectedLinks puts the selected jobs' source links on the clipboard,
// one per line, each once: pasted back into Siphon they add the same files.
func (q *queueTab) copySelectedLinks() {
	links := sourceLinks(q.selectedJobs())
	if len(links) == 0 {
		return
	}
	fyne.CurrentApp().Clipboard().SetContent(strings.Join(links, "\n"))
	if len(links) == 1 {
		q.vm.Notify("Link copied.")
	} else {
		q.vm.Notify(fmt.Sprintf("%d links copied.", len(links)))
	}
}

// sourceLinks are the jobs' source links, each once, without the prefix
// that sends a page to yt-dlp or gallery-dl: that is Siphon's own marker,
// not part of the link.
func sourceLinks(jobs []queue.Job) []string {
	seen := map[string]bool{}
	var out []string
	for _, j := range jobs {
		l := strings.TrimPrefix(strings.TrimPrefix(j.SourcePage, site.ViaYtDlp), site.ViaGalleryDL)
		if l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

// removeSelected takes the selected jobs out of the queue. If any has a
// partial file it asks first, offering to delete those files; finished
// files are never touched.
func (q *queueTab) removeSelected() {
	jobs := q.selectedJobs()
	if len(jobs) == 0 {
		return
	}
	remove := func(wipe bool) {
		for _, j := range jobs {
			q.eng.Remove(j.ID, wipe && !j.State.Finished())
		}
		q.sel.clear()
		q.vm.Replace(q.eng.Jobs())
		q.render()
	}
	a := actionsFor(jobs)
	if a.partial == 0 {
		remove(true)
		return
	}
	what := shortName(jobs[0].Filename)
	if len(jobs) > 1 {
		what = fmt.Sprintf("%d downloads", len(jobs))
	}
	msg := widget.NewLabel(fmt.Sprintf("Remove %s from the queue?\n%s downloaded so far. Finished files are not touched.", what, humanBytes(a.partial)))
	msg.Wrapping = fyne.TextWrapWord
	wipe := widget.NewCheck("Also delete the partially downloaded files", nil)
	wipe.SetChecked(true)
	hint := widget.NewLabel("Unchecked, they stay: adding the same link later continues where they stopped.")
	hint.Wrapping = fyne.TextWrapWord
	hint.Importance = widget.LowImportance
	d := dialog.NewCustomConfirm("Remove", "Remove", "Back", container.NewVBox(msg, wipe, hint), func(ok bool) {
		if ok {
			remove(wipe.Checked)
		}
	}, q.win)
	d.Show()
}

// showInFolder opens a job's folder: a finished file selected, a gallery's
// own folder, a partial download's folder, or where it will go.
func (q *queueTab) showInFolder(j queue.Job) {
	target, sel := j.Path, true
	if fi, err := os.Stat(j.Path); j.Path == "" || err != nil {
		target, sel = filepath.Join(j.OutDir, dirOnDisk(j.Dir)), false
		if d := filepath.Dir(j.Path); j.Path != "" && isDir(d) {
			target = d
		}
		if !isDir(target) {
			target = j.OutDir // nothing written yet
		}
	} else if fi.IsDir() {
		sel = false
	}
	if err := openInExplorer(target, sel); err != nil {
		q.vm.Notify("Could not open the folder: " + err.Error())
	}
}

// dirOnDisk turns a job's folder ("/" between levels) into a path.
func dirOnDisk(dir string) string {
	return filepath.FromSlash(strings.Trim(dir, "/"))
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func (q *queueTab) togglePauseAll() {
	// While a captcha hold is on the button says "resume"; pressing it must lift the hold.
	if q.eng.Paused() || len(q.eng.CaptchaHeld()) > 0 {
		q.eng.ResumeAll()
	} else {
		q.eng.PauseAll()
	}
	q.vm.Replace(q.eng.Jobs())
	q.render()
}

// openFolder opens the selected download's folder when exactly one is
// selected; otherwise the last downloaded file selected in its folder, or
// the output root.
func (q *queueTab) openFolder() {
	if jobs := q.selectedJobs(); len(jobs) == 1 {
		q.showInFolder(jobs[0])
		return
	}
	var lastPath, lastDir string
	for _, r := range q.vm.Rows() {
		j := r.Job
		switch j.State {
		case queue.StateDone, queue.StateSkipped:
			if j.Path != "" {
				if fi, err := os.Stat(j.Path); err == nil && fi.IsDir() {
					lastPath, lastDir = "", j.Path // a gallery: its folder
				} else {
					lastPath = j.Path
				}
			}
		case queue.StateRunning, queue.StatePaused:
			if j.Path != "" {
				lastDir = filepath.Dir(j.Path)
			}
		}
	}
	target, sel, reason := pickOpenTarget(lastPath, lastDir, normalizeDir(q.defaultDir()))
	if target == "" {
		if reason != "" {
			q.vm.Notify(reason)
		}
		return
	}
	if err := openInExplorer(target, sel); err != nil {
		q.vm.Notify("Could not open the folder: " + err.Error())
	}
}
