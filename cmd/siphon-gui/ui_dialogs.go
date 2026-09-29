package main

import (
	"context"
	"fmt"
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

// The dialogs of the main window: Add links, Settings, and the questions
// asked before cancelling everything or running the VPN command.

// buildSettings makes the Settings dialog's form and applies the saved
// values to the engine. Each change takes effect right away and is saved.
func (q *queueTab) buildSettings() fyne.CanvasObject {
	prefs, eng := q.prefs, q.eng

	q.folder = widget.NewEntry()
	q.folder.SetText(q.defaultDir())
	q.folder.OnChanged = func(s string) { prefs.SetString(prefOutDir, normalizeDir(s)) }
	browse := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() { q.pickFolder(q.folder) })

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

	// VPN switch command: runs when mega's per-IP quota runs out
	// (MegaBasterd's "run command on 509"). EMPTY IS NORMAL: a notification
	// arrives, the user switches the VPN from its own app, and the queue
	// notices within ~10 s and goes on.
	q.quotaCmd = widget.NewEntry()
	q.quotaCmd.SetPlaceHolder("optional")
	if saved := prefs.String(prefQuotaCmd); saved != "" {
		q.quotaCmd.SetText(saved)
		eng.SetQuotaCommand(saved)
	}
	q.quotaCmd.OnChanged = func(v string) {
		eng.SetQuotaCommand(v)
		prefs.SetString(prefQuotaCmd, strings.TrimSpace(v))
	}
	test := widget.NewButton("Test", q.tryQuotaCommand)

	item := func(label string, obj fyne.CanvasObject, hint string) *widget.FormItem {
		it := widget.NewFormItem(label, obj)
		it.HintText = hint
		return it
	}
	return widget.NewForm(
		item("Download folder", container.NewBorder(nil, nil, nil, browse, q.folder),
			"Where new links are saved; the Add dialog can change it."),
		item("Speed limit", container.NewBorder(nil, nil, nil, widget.NewLabel("MB/s"), q.speed),
			"For all downloads together; 0 or empty is no limit."),
		item("Downloads at once", active, "Files downloading at the same time."),
		item("Connections per file", q.segments,
			"A site may allow fewer: bunkr 3, pixeldrain 1. Each row shows what it really gets."),
		item("VPN switch command", container.NewBorder(nil, nil, nil, test, q.quotaCmd),
			"For mega's quota, optional: runs when it runs out. Empty: you get a notification and switch the VPN yourself."),
	)
}

// defaultDir is the saved download folder, or the default one.
func (q *queueTab) defaultDir() string {
	if saved := q.prefs.String(prefOutDir); saved != "" {
		return saved
	}
	return defaultOutDir()
}

// pickFolder lets the user choose a folder into entry.
func (q *queueTab) pickFolder(entry *widget.Entry) {
	dialog.ShowFolderOpen(func(lu fyne.ListableURI, err error) {
		if err != nil || lu == nil {
			return
		}
		// Path() comes with FORWARD SLASHES; written into the box without
		// normalizing, "Open folder" opens Documents. Details in normalizeDir.
		entry.SetText(normalizeDir(lu.Path()))
	}, q.win)
}

func (q *queueTab) showSettings() {
	q.folder.SetText(q.defaultDir()) // the Add dialog may have changed it
	d := dialog.NewCustom("Settings", "Close", q.settings, q.win)
	d.Resize(fyne.NewSize(680, 0))
	d.Show()
}

// showAddDialog asks for links and the folder they go to. extra is added
// to the box (a paste); links that couldn't be added last time are there
// already.
func (q *queueTab) showAddDialog(extra string) {
	entry := widget.NewMultiLineEntry()
	entry.Wrapping = fyne.TextWrapOff
	entry.SetMinRowsVisible(8)
	entry.SetPlaceHolder("One link per line: mega, gofile, mediafire, pixeldrain, bunkr, cyberdrop,\n" +
		"any direct file link, and video or gallery pages if yt-dlp / gallery-dl is installed")
	entry.SetText(strings.TrimSpace(strings.Join([]string{q.pending, strings.TrimSpace(extra)}, "\n")))
	folder := widget.NewEntry()
	folder.SetText(q.defaultDir())
	browse := widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() { q.pickFolder(folder) })
	content := container.NewBorder(nil,
		container.NewBorder(nil, nil, widget.NewLabel("Save to"), browse, folder),
		nil, nil, entry)
	q.addEntry = entry
	d := dialog.NewCustomConfirm("Add links", "Add", "Cancel", content, func(ok bool) {
		q.addEntry = nil
		if ok {
			q.addLinks(entry.Text, folder.Text)
		}
	}, q.win)
	d.Resize(fyne.NewSize(760, 0))
	d.Show()
	q.win.Canvas().Focus(entry)
}

// addLinks resolves the links in the background and adds their files to
// the queue, saving to dir (which becomes the default folder). Resolution
// needs the network; it runs in a goroutine so it doesn't lock the UI.
func (q *queueTab) addLinks(text, dir string) {
	urls := parseLinks(text)
	if len(urls) == 0 {
		dialog.ShowInformation("No links", "Paste at least one link first. One link per line.", q.win)
		return
	}
	outDir := normalizeDir(dir)
	if outDir == "" {
		q.pending = text
		dialog.ShowInformation("No folder", "Choose a folder to save to.", q.win)
		return
	}
	q.prefs.SetString(prefOutDir, outDir)
	q.pending = ""
	q.addBtn.Disable()
	q.vm.Notify(fmt.Sprintf("Resolving %d links...", len(urls)))

	go func() {
		added, errs, failed := q.resolveLinks(urls, outDir)
		q.vm.Replace(q.eng.Jobs())
		msg := addSummary(added, errs)
		if len(failed) > 0 {
			msg += " (Add links opens with them.)"
		}
		q.vm.Notify(msg)
		fyne.Do(func() {
			q.addBtn.Enable()
			// Links that couldn't be added wait in the Add dialog; the user
			// fixes and retries them without copying.
			q.pending = strings.Join(failed, "\n")
			q.render()
		})
	}()
}

// resolveLinks adds each link's files to the queue and says what happened.
func (q *queueTab) resolveLinks(urls []string, outDir string) (added int, errs, failed []string) {
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
	return added, errs, failed
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
