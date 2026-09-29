package main

import (
	"context"
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

	"github.com/mustafaberatdemirci/siphon/internal/hook"
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

// queueTab is the "Download" tab: adding links, the queue table with its
// filters, the toolbar and the status line.
type queueTab struct {
	win   fyne.Window
	prefs fyne.Preferences
	eng   *queue.Engine
	vm    *viewModel

	links    *widget.Entry
	outDir   *widget.Entry
	speed    *widget.Entry
	quotaCmd *widget.Entry
	segments *widget.Select

	banner     *fyne.Container
	bannerText *widget.Label
	addBtn     *widget.Button
	resumeSel  *widget.Button
	pauseSel   *widget.Button
	removeSel  *widget.Button
	pauseAll   *widget.Button
	cancelAll  *widget.Button
	list       *widget.List
	sidebar    *widget.List
	status     *widget.Label
	sortArrows func(sortOrder)

	// What the table shows, taken on the UI thread at each render so the
	// list's callbacks read one consistent picture.
	filter     filter
	order      sortOrder
	visible    []row
	visibleIDs []string
	entries    []sidebarEntry
	sel        *selection

	// shown reports whether the Download tab is the one on screen: the
	// keyboard shortcuts act on the table only then. nil means always.
	shown func() bool

	// onQuota is told, on the UI thread, whether downloads wait for quota
	// (the notification-area icon changes). May be nil.
	onQuota func(waiting bool)
}

func newQueueTab(win fyne.Window, prefs fyne.Preferences, eng *queue.Engine, vm *viewModel, onQuota func(bool)) (*queueTab, fyne.CanvasObject) {
	q := &queueTab{win: win, prefs: prefs, eng: eng, vm: vm, onQuota: onQuota, sel: newSelection()}

	// --- Link input ---
	q.links = widget.NewMultiLineEntry()
	q.links.SetPlaceHolder("Paste links — one per line: mega, gofile, mediafire, pixeldrain, bunkr, cyberdrop,\n" +
		"any direct file link, and video or gallery pages if yt-dlp / gallery-dl is installed")
	q.links.Wrapping = fyne.TextWrapOff
	q.addBtn = widget.NewButtonWithIcon("Add", theme.ContentAddIcon(), q.add)
	q.addBtn.Importance = widget.HighImportance
	linkBox := container.NewVScroll(q.links)
	linkBox.SetMinSize(fyne.NewSize(0, 64))

	// --- Folder ---
	q.outDir = widget.NewEntry()
	if saved := prefs.String(prefOutDir); saved != "" {
		q.outDir.SetText(saved)
	} else {
		q.outDir.SetText(defaultOutDir())
	}
	q.outDir.OnChanged = func(s string) { prefs.SetString(prefOutDir, normalizeDir(s)) }
	pick := widget.NewButton("Browse...", func() {
		dialog.ShowFolderOpen(func(lu fyne.ListableURI, err error) {
			if err != nil || lu == nil {
				return
			}
			// Path() comes with FORWARD SLASHES; written into the box without
			// normalizing, "Open folder" opens Documents. Details in normalizeDir.
			q.outDir.SetText(normalizeDir(lu.Path()))
		}, win)
	})

	// --- Toolbar ---
	// The first group acts on the selected rows (as the row menu does), the
	// second on the whole queue.
	q.resumeSel = widget.NewButtonWithIcon("Resume", theme.MediaPlayIcon(), q.resumeSelected)
	q.pauseSel = widget.NewButtonWithIcon("Pause", theme.MediaPauseIcon(), q.pauseSelected)
	q.removeSel = widget.NewButtonWithIcon("Remove", theme.DeleteIcon(), q.removeSelected)
	q.pauseAll = widget.NewButtonWithIcon("Pause all", theme.MediaPauseIcon(), q.togglePauseAll)
	// "Pause all" keeps the queue for later; this empties it of everything
	// unfinished. It asks first and offers to delete the partial files.
	q.cancelAll = widget.NewButtonWithIcon("Cancel all", theme.CancelIcon(), q.confirmCancelAll)
	q.cancelAll.Disable()
	clearBtn := widget.NewButtonWithIcon("Clear finished", theme.ContentClearIcon(), func() {
		eng.ClearFinished()
		vm.Replace(eng.Jobs())
		q.render()
	})
	// "Resume all" only covers paused jobs; the user shouldn't have to resume
	// one by one dozens of jobs that failed because of the network or site.
	retryFailedBtn := widget.NewButtonWithIcon("Retry failed", theme.ViewRefreshIcon(), func() {
		eng.RetryFailed()
		vm.Replace(eng.Jobs())
		q.render()
	})
	openBtn := widget.NewButtonWithIcon("Open folder", theme.FolderOpenIcon(), q.openFolder)

	q.speed = widget.NewEntry()
	q.speed.SetPlaceHolder("0")
	if v := prefs.Float(prefSpeedLimit); v > 0 {
		q.speed.SetText(strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), "."))
		eng.SetSpeedLimit(int64(v * 1024 * 1024))
	}
	q.speed.OnChanged = func(s string) {
		bps, err := parseSpeedLimit(s)
		if err != nil {
			q.vm.Notify(err.Error())
			return
		}
		eng.SetSpeedLimit(bps)
		prefs.SetFloat(prefSpeedLimit, float64(bps)/(1024*1024))
	}
	speedBox := container.NewHBox(widget.NewLabel("Speed limit"), container.NewGridWrap(fyne.NewSize(70, 36), q.speed), widget.NewLabel("MB/s"))

	// Downloads at once, across sites (MegaBasterd's default is 4 too). A
	// site's own max_concurrent still caps its share. Changing it takes
	// effect right away: raising starts more jobs, lowering lets running
	// ones finish and starts no new ones until fewer run.
	active := widget.NewSelect([]string{"1", "2", "3", "4", "5", "6", "8"}, nil)
	active.SetSelected(fmt.Sprint(eng.MaxActive()))
	active.OnChanged = func(v string) {
		n := 1
		fmt.Sscanf(v, "%d", &n)
		eng.SetMaxActive(n)
		prefs.SetInt(prefMaxActive, n)
	}
	activeBox := container.NewHBox(widget.NewLabel("Downloads at once"), active)

	// Connections per file: segmented download. The site ceiling
	// (max_segments) clips it: 1 on pixeldrain, 3 on bunkr, 8 on mega. The
	// user choice is the "request", the ceiling the "permission"; picking a
	// number says which sites cap it, and every running row shows the
	// connections its download really got. It reaches running downloads too.
	q.segments = widget.NewSelect([]string{"1", "2", "4", "6", "8"}, nil)
	if n := prefs.IntWithFallback(prefSegments, queue.DefaultSegments); n > 0 {
		q.segments.SetSelected(fmt.Sprint(n))
		eng.SetSegments(n)
	}
	// Wired after the saved value is restored: restoring isn't a choice the
	// user made, so it shouldn't produce a notice.
	q.segments.OnChanged = func(v string) {
		n := 1
		fmt.Sscanf(v, "%d", &n)
		eng.SetSegments(n)
		prefs.SetInt(prefSegments, n)
		q.vm.Notify(segmentsNotice(n, eng.SegmentCeilings()))
	}
	segBox := container.NewHBox(widget.NewLabel("Connections/file"), q.segments)

	toolbar := container.NewVBox(
		container.NewHBox(q.resumeSel, q.pauseSel, q.removeSel, widget.NewSeparator(),
			q.pauseAll, q.cancelAll, retryFailedBtn, clearBtn, openBtn),
		container.NewHBox(speedBox, widget.NewLabel("  "), activeBox, widget.NewLabel("  "), segBox),
	)

	// --- VPN switch command (optional) ---
	// Runs when mega's per-IP quota runs out; a command that switches VPN
	// server (MegaBasterd's "run command on 509" feature). EMPTY IS NORMAL:
	// then a system notification arrives when the quota runs out, the user
	// switches the VPN from its own app, and the queue notices within ~10 s and
	// goes on. The box is only for those who want to automate that step.
	q.quotaCmd = widget.NewEntry()
	q.quotaCmd.SetPlaceHolder("optional — if empty you get a notification when the quota runs out and switch the VPN yourself")
	if saved := prefs.String(prefQuotaCmd); saved != "" {
		q.quotaCmd.SetText(saved)
		eng.SetQuotaCommand(saved)
	}
	q.quotaCmd.OnChanged = func(v string) {
		eng.SetQuotaCommand(v)
		prefs.SetString(prefQuotaCmd, strings.TrimSpace(v))
	}
	tryBtn := widget.NewButton("Test", q.tryQuotaCommand)
	quotaRow := container.NewBorder(nil, nil, widget.NewLabel("VPN switch command"), tryBtn, q.quotaCmd)
	// Not on the main screen: most users (VPNs without a command line) get
	// no use from it and it was confusing (measured).
	advanced := widget.NewAccordion(widget.NewAccordionItem("Advanced", quotaRow))

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

	top := container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("Links"), q.addBtn, linkBox),
		container.NewBorder(nil, nil, widget.NewLabel("Folder"), pick, q.outDir),
		toolbar,
		advanced,
		q.banner,
	)
	body := container.NewBorder(nil, nil, container.NewHBox(sidebar, widget.NewSeparator()), nil, table)
	root := container.NewBorder(top, q.status, nil, nil, body)

	q.bindKeys()
	q.render()
	if autoRefresh {
		go q.refreshLoop()
	}
	return q, root
}

// bindKeys: Delete removes the selected rows, Ctrl+A selects every row
// shown. A text box with the focus keeps its own Delete and Ctrl+A.
func (q *queueTab) bindKeys() {
	c := q.win.Canvas()
	onTable := func() bool { return q.shown == nil || q.shown() }
	c.SetOnTypedKey(func(ev *fyne.KeyEvent) {
		if ev.Name == fyne.KeyDelete && onTable() {
			q.removeSelected()
		}
	})
	c.AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyA, Modifier: fyne.KeyModifierShortcutDefault}, func(fyne.Shortcut) {
		if onTable() {
			q.sel.all(q.visibleIDs)
			q.renderSelection()
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

	if unfinished, _ := q.vm.Unfinished(); unfinished > 0 {
		q.cancelAll.Enable()
	} else {
		q.cancelAll.Disable()
	}
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

// add resolves the entered links in the background and adds them to the
// queue. Resolution needs the network; it runs in a goroutine so it doesn't
// lock the UI.
func (q *queueTab) add() {
	urls := parseLinks(q.links.Text)
	if len(urls) == 0 {
		dialog.ShowInformation("No links", "Paste at least one link first. One link per line.", q.win)
		return
	}
	outDir := normalizeDir(q.outDir.Text)
	if outDir == "" {
		dialog.ShowInformation("No folder", "Choose an output folder.", q.win)
		return
	}
	q.addBtn.Disable()
	q.vm.Notify(fmt.Sprintf("Resolving %d links...", len(urls)))

	go func() {
		var added int
		var errs, failed []string
		for _, u := range urls {
			n, err := q.eng.Add(context.Background(), u, outDir)
			added += n
			switch {
			case err != nil:
				errs = append(errs, firstLine(err.Error()))
				failed = append(failed, u)
			case n == 0:
				// Resolution finished without error but no files came out: an
				// empty folder or every file was skipped. "0 files added"
				// doesn't say why.
				errs = append(errs, "no files to download found: "+u)
				failed = append(failed, u)
			}
		}
		q.vm.Replace(q.eng.Jobs())
		q.vm.Notify(addSummary(added, errs))
		fyne.Do(func() {
			q.addBtn.Enable()
			// Links that couldn't be added stay in the box; the user fixes and
			// retries them without copying. Added ones are cleared.
			q.links.SetText(strings.Join(failed, "\n"))
		})
	}()
}

// addSummary is the one-line result of Add.
func addSummary(added int, errs []string) string {
	switch {
	case len(errs) > 0 && added == 0:
		return "Could not add: " + strings.Join(errs, " | ")
	case len(errs) > 0:
		return fmt.Sprintf("%d files added; %d links could not be added: %s", added, len(errs), strings.Join(errs, " | "))
	default:
		return fmt.Sprintf("%d files added to the queue.", added)
	}
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

// confirmCancelAll asks before emptying the queue of every unfinished job.
// Deleting the partial files is offered, checked: cancelling a download
// normally means throwing away what it left behind. Unchecked, the files stay
// and adding the same link later continues where it stopped.
func (q *queueTab) confirmCancelAll() {
	n, partial := q.vm.Unfinished()
	if n == 0 {
		q.vm.Notify("Nothing to cancel: every download in the list is finished.")
		return
	}
	noun := "downloads"
	if n == 1 {
		noun = "download"
	}
	msg := widget.NewLabel(fmt.Sprintf("Stop and remove %d unfinished %s from the queue?\nFinished files are not touched.", n, noun))
	msg.Wrapping = fyne.TextWrapWord
	wipe := widget.NewCheck(fmt.Sprintf("Also delete the partially downloaded files (%s)", humanBytes(partial)), nil)
	wipe.SetChecked(true)
	content := container.NewVBox(msg)
	if partial > 0 {
		content.Add(wipe)
	}
	d := dialog.NewCustomConfirm("Cancel all", "Cancel all", "Back", content, func(ok bool) {
		if !ok {
			return
		}
		deleted := wipe.Checked
		canceled := q.eng.CancelAll(deleted)
		q.vm.Replace(q.eng.Jobs())
		q.render()
		switch {
		case partial > 0 && deleted:
			q.vm.Notify(fmt.Sprintf("Canceled %d %s; partial files deleted.", canceled, noun))
		case partial > 0:
			q.vm.Notify(fmt.Sprintf("Canceled %d %s; partial files kept (add the same link to continue).", canceled, noun))
		default:
			q.vm.Notify(fmt.Sprintf("Canceled %d %s.", canceled, noun))
		}
	}, q.win)
	d.Show()
}

// tryQuotaCommand runs the command in the box now and shows the result: the
// user sees the command is right without waiting for the quota to run out.
func (q *queueTab) tryQuotaCommand() {
	line := strings.TrimSpace(q.quotaCmd.Text)
	if line == "" {
		dialog.ShowInformation("No command",
			"This box is optional. If you leave it empty you get a notification when the quota runs out; you switch the VPN from its own app and downloads resume by themselves.\n\n"+
				"If you fill it in, Siphon runs this command for you when the quota runs out. It depends on which VPN you use; e.g. NordVPN: \"C:\\Program Files\\NordVPN\\NordVPN.exe\" -c",
			q.win)
		return
	}
	q.vm.Notify("Testing the command: " + line)
	go func() {
		start := time.Now()
		out, err := hook.Run(context.Background(), line, 0)
		took := time.Since(start).Round(time.Second)
		fyne.Do(func() {
			switch {
			case err != nil && out != "":
				dialog.ShowError(fmt.Errorf("command failed (%v):\n\n%s", err, out), q.win)
				q.vm.Notify("Command failed: " + firstLine(err.Error()))
			case err != nil:
				dialog.ShowError(fmt.Errorf("command failed: %v", err), q.win)
				q.vm.Notify("Command failed: " + firstLine(err.Error()))
			default:
				msg := fmt.Sprintf("The command finished in %s.", took)
				if out != "" {
					msg += "\n\nOutput:\n" + out
				}
				dialog.ShowInformation("Command ran", msg, q.win)
				q.vm.Notify(fmt.Sprintf("Command ran (%s).", took))
			}
		})
	}()
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
	target, sel, reason := pickOpenTarget(lastPath, lastDir, normalizeDir(q.outDir.Text))
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
