package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/hook"
	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

// refreshEvery is how often at most the list is redrawn. Drawing per event
// meant hundreds of refreshes per second with 20 parallel downloads; here it
// is drawn if something changed, otherwise not at all.
const refreshEvery = 150 * time.Millisecond

const (
	prefOutDir     = "out_dir"
	prefSpeedLimit = "speed_limit_mbps"
	prefSegments   = "segments"
	prefMaxActive  = "max_active"
	prefQuotaCmd   = "quota_command"
)

// queueTab is the "Download" tab: adding links, the queue list, per-row
// controls, global buttons and the status line.
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
	pauseAll   *widget.Button
	cancelAll  *widget.Button
	list       *widget.List
	status     *widget.Label

	// onQuota is told, on the UI thread, whether downloads wait for quota
	// (the notification-area icon changes). May be nil.
	onQuota func(waiting bool)
}

func newQueueTab(win fyne.Window, prefs fyne.Preferences, eng *queue.Engine, vm *viewModel, onQuota func(bool)) (*queueTab, fyne.CanvasObject) {
	q := &queueTab{win: win, prefs: prefs, eng: eng, vm: vm, onQuota: onQuota}

	// --- Link input ---
	q.links = widget.NewMultiLineEntry()
	q.links.SetPlaceHolder("Paste links — one per line: mega, gofile, mediafire, pixeldrain, bunkr, cyberdrop,\n" +
		"any direct file link, and video or gallery pages if yt-dlp / gallery-dl is installed")
	q.links.Wrapping = fyne.TextWrapOff
	q.addBtn = widget.NewButton("Add", q.add)
	q.addBtn.Importance = widget.HighImportance
	linkBox := container.NewVScroll(q.links)
	linkBox.SetMinSize(fyne.NewSize(0, 84))

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
	q.pauseAll = widget.NewButton("⏸ Pause all", q.togglePauseAll)
	// "Pause all" keeps the queue for later; this empties it of everything
	// unfinished. It asks first and offers to delete the partial files.
	q.cancelAll = widget.NewButton("✕ Cancel all", q.confirmCancelAll)
	q.cancelAll.Disable()
	clearBtn := widget.NewButton("Clear finished", func() {
		eng.ClearFinished()
		vm.Replace(eng.Jobs())
	})
	// "Resume all" only covers paused jobs; the user shouldn't have to press
	// ▶ one by one on dozens of jobs that failed because of the network or site.
	retryFailedBtn := widget.NewButton("Retry failed", func() {
		eng.RetryFailed()
		vm.Replace(eng.Jobs())
	})
	openBtn := widget.NewButton("Open folder", q.openFolder)

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
		container.NewHBox(q.pauseAll, q.cancelAll, retryFailedBtn, clearBtn, openBtn),
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
	// Sits above the list while jobs wait for quota; independent of the
	// notification settings, the first thing seen when the window opens.
	// "Try now" is for the user who switched the VPN and doesn't want to wait
	// for the next probe (~10 s).
	q.bannerText = widget.NewLabel("")
	q.bannerText.Wrapping = fyne.TextWrapWord
	q.bannerText.TextStyle = fyne.TextStyle{Bold: true}
	retryBtn := widget.NewButton("Try now", func() {
		eng.RetryWaiting()
		vm.Replace(eng.Jobs())
	})
	retryBtn.Importance = widget.HighImportance
	q.banner = container.NewBorder(nil, nil, widget.NewIcon(theme.WarningIcon()), retryBtn, q.bannerText)
	q.banner.Hide()

	// --- List ---
	q.list = widget.NewList(
		func() int { return vm.Len() },
		func() fyne.CanvasObject { return newJobRow() },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			r, ok := vm.Row(i)
			if !ok {
				return
			}
			o.(*jobRow).set(r, q)
		},
	)

	q.status = widget.NewLabel(vm.StatusLine())
	q.status.Wrapping = fyne.TextWrapWord

	top := container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("Links"), q.addBtn, linkBox),
		container.NewBorder(nil, nil, widget.NewLabel("Folder"), pick, q.outDir),
		toolbar,
		advanced,
		q.banner,
	)
	root := container.NewBorder(top, q.status, nil, nil, q.list)

	go q.refreshLoop()
	return q, root
}

// refreshLoop redraws the list and the status line if the model changed.
func (q *queueTab) refreshLoop() {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for range t.C {
		if !q.vm.TakeDirty() {
			continue
		}
		summary := q.vm.StatusLine()
		paused := q.eng.Paused()
		captcha := q.eng.CaptchaHeld()
		banner := q.vm.QuotaBanner()
		unfinished, _ := q.vm.Unfinished()
		fyne.Do(func() {
			q.list.Refresh()
			q.status.SetText(summary)
			if unfinished > 0 {
				q.cancelAll.Enable()
			} else {
				q.cancelAll.Disable()
			}
			if banner == "" {
				q.banner.Hide()
			} else {
				q.bannerText.SetText(banner)
				q.banner.Show()
			}
			if q.onQuota != nil {
				q.onQuota(banner != "")
			}
			switch {
			case paused:
				q.pauseAll.SetText("▶ Resume all")
			case len(captcha) > 0:
				// The reason must be visible: a captcha held that site's jobs
				// (the other sites go on); it resumes when the user presses ▶
				// on one of that site's jobs or presses here.
				q.pauseAll.SetText("▶ Captcha: " + strings.Join(captcha, ", ") + " paused — resume")
			default:
				q.pauseAll.SetText("⏸ Pause all")
			}
		})
	}
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
		q.pauseAll.SetText("⏸ Pause all")
	} else {
		q.eng.PauseAll()
		q.pauseAll.SetText("▶ Resume all")
	}
	q.vm.Replace(q.eng.Jobs())
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

// openFolder opens the last downloaded file selected in its folder; otherwise the output root.
func (q *queueTab) openFolder() {
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

// removeJob is the row's ✕: if there is a partial file it asks what to do.
func (q *queueTab) removeJob(j queue.Job) {
	if j.State.Finished() || j.Done == 0 {
		q.eng.Remove(j.ID, !j.State.Finished())
		q.vm.Replace(q.eng.Jobs())
		return
	}
	d := dialog.NewConfirm("Remove from queue",
		fmt.Sprintf("%s\n\n%s downloaded so far. Delete the partial file?\n(No: the file stays and you can continue later with the same link.)",
			shortName(j.Filename), humanBytes(j.Done)),
		func(wipe bool) {
			q.eng.Remove(j.ID, wipe)
			q.vm.Replace(q.eng.Jobs())
		}, q.win)
	d.SetConfirmText("Delete")
	d.SetDismissText("Keep")
	d.Show()
}

// ---------- Row widget ----------

// jobRow is a single row in the list: name, info, progress bar, two buttons.
// Fyne recycles rows, so set() rebinds the buttons' targets on every call.
type jobRow struct {
	widget.BaseWidget
	name   *widget.Label
	meta   *widget.Label
	bar    *widget.ProgressBar
	action *widget.Button
	remove *widget.Button
	root   fyne.CanvasObject
}

func newJobRow() *jobRow {
	r := &jobRow{
		name:   widget.NewLabel(""),
		meta:   widget.NewLabel(""),
		bar:    widget.NewProgressBar(),
		action: widget.NewButton("⏸", nil),
		remove: widget.NewButton("✕", nil),
	}
	r.name.Truncation = fyne.TextTruncateEllipsis
	r.name.TextStyle = fyne.TextStyle{Bold: true}
	r.meta.Alignment = fyne.TextAlignTrailing
	r.remove.Importance = widget.LowImportance
	top := container.NewBorder(nil, nil, nil, r.meta, r.name)
	body := container.NewVBox(top, r.bar)
	r.root = container.NewBorder(nil, nil, nil, container.NewHBox(r.action, r.remove), body)
	r.ExtendBaseWidget(r)
	return r
}

func (r *jobRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(r.root)
}

func (r *jobRow) set(row row, q *queueTab) {
	j := row.Job
	r.name.SetText(shortName(j.Filename))
	r.meta.SetText(rowMeta(row))
	r.bar.SetValue(rowProgress(j))

	label, act := actionFor(j.State)
	r.action.SetText(label)
	switch act {
	case actionPause:
		r.action.Enable()
		r.action.OnTapped = func() { q.eng.Pause(j.ID); q.vm.Replace(q.eng.Jobs()) }
	case actionResume:
		r.action.Enable()
		r.action.OnTapped = func() { q.eng.Resume(j.ID); q.vm.Replace(q.eng.Jobs()) }
	default:
		r.action.OnTapped = nil
		r.action.Disable()
	}
	r.remove.OnTapped = func() { q.removeJob(j) }
}
