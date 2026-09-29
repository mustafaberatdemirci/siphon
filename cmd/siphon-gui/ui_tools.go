package main

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/external"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/run"
)

// toolsStatus is the "tools" block of the Diagnose tab: what was found.
func toolsStatus() string {
	return "tools (for pages of sites Siphon doesn't know)\n" +
		strings.Join(external.Find(run.ToolPaths("")).Lines(), "\n")
}

// toolLabel is one line of the install dialog: the tool, its size once the
// release was asked, what it is for, and whether it is already there.
func toolLabel(p external.Package, foundPath string, resolveErr error) string {
	size := "size unknown"
	if p.Size > 0 {
		size = humanBytes(p.Size)
	}
	s := fmt.Sprintf("%s (%s) — %s", p.Tool, size, p.Purpose)
	switch {
	case resolveErr != nil:
		s += " — can't be installed now: " + firstLine(resolveErr.Error())
	case foundPath != "":
		s += " — installed; tick to update"
	}
	return s
}

// showInstallTools is "Install tools…": what Siphon can install on this
// system, what each one is for and how big, the missing ones ticked. Nothing
// is downloaded before the user presses Install. onDone runs afterwards.
func showInstallTools(win fyne.Window, onDone func()) {
	pkgs := external.Packages(runtime.GOOS, runtime.GOARCH)
	if len(pkgs) == 0 {
		dialog.ShowInformation("Install tools",
			"Siphon has no official builds to install on this system.\nInstall yt-dlp, gallery-dl and ffmpeg with your package manager.", win)
		return
	}
	dir, err := external.ToolsDir()
	if err != nil {
		dialog.ShowError(fmt.Errorf("no folder for the tools: %w", err), win)
		return
	}
	found := external.Find(run.ToolPaths(""))

	errs := make([]error, len(pkgs))
	checks := make([]*widget.Check, len(pkgs))
	list := container.NewVBox()
	for i, p := range pkgs {
		checks[i] = widget.NewCheck(toolLabel(p, found.Path(p.Tool), nil), nil)
		checks[i].SetChecked(found.Path(p.Tool) == "")
		list.Add(checks[i])
	}
	intro := widget.NewLabel("Each tool is downloaded from its own official release, checked against the " +
		"SHA-256 published with it, and put in\n" + dir + "\nNothing is installed if a check fails.")
	intro.Wrapping = fyne.TextWrapWord

	// Ask the releases for sizes (and checksums) while the user reads.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		client := snet.NewClient()
		for i := range pkgs {
			p := pkgs[i]
			err := p.Resolve(ctx, client)
			if ctx.Err() != nil {
				return
			}
			fyne.Do(func() {
				if err == nil {
					pkgs[i] = p
				}
				errs[i] = err
				checks[i].Text = toolLabel(pkgs[i], found.Path(p.Tool), err)
				if err != nil {
					checks[i].SetChecked(false)
					checks[i].Disable()
				}
				checks[i].Refresh()
			})
		}
	}()

	d := dialog.NewCustomConfirm("Install tools", "Install", "Cancel", container.NewVBox(intro, list), func(ok bool) {
		cancel()
		if !ok {
			return
		}
		var chosen []external.Package
		for i, c := range checks {
			if c.Checked && errs[i] == nil {
				chosen = append(chosen, pkgs[i])
			}
		}
		if len(chosen) > 0 {
			installTools(win, dir, chosen, onDone)
		}
	}, win)
	d.Resize(fyne.NewSize(640, 0))
	d.Show()
}

// installTools installs the chosen packages one after another with a
// progress bar; Cancel stops the download (nothing half-installed is left).
func installTools(win fyne.Window, dir string, chosen []external.Package, onDone func()) {
	status := widget.NewLabel("")
	bar := widget.NewProgressBar()
	ctx, cancel := context.WithCancel(context.Background())
	pd := dialog.NewCustom("Installing tools", "Cancel", container.NewVBox(status, bar), win)
	pd.SetOnClosed(cancel)
	pd.Resize(fyne.NewSize(480, 0))
	pd.Show()

	go func() {
		client := snet.NewClient()
		var report []string
		for _, p := range chosen {
			if p.URL == "" {
				if err := p.Resolve(ctx, client); err != nil {
					report = append(report, firstLine(err.Error()))
					continue
				}
			}
			label := fmt.Sprintf("Downloading %s (%s)…", p.Tool, humanBytes(p.Size))
			fyne.Do(func() {
				status.SetText(label)
				bar.SetValue(0)
			})
			var last time.Time
			err := external.Install(ctx, client, dir, p, func(done, total int64) {
				if total <= 0 || time.Since(last) < 100*time.Millisecond {
					return
				}
				last = time.Now()
				v := float64(done) / float64(total)
				fyne.Do(func() { bar.SetValue(v) })
			})
			switch {
			case ctx.Err() != nil:
				report = append(report, p.Tool+": canceled")
			case err != nil:
				report = append(report, firstLine(err.Error()))
			default:
				report = append(report, p.Tool+": installed, SHA-256 checked")
			}
			if ctx.Err() != nil {
				break
			}
		}
		fyne.Do(func() {
			pd.Hide()
			dialog.ShowInformation("Install tools", strings.Join(report, "\n"), win)
			if onDone != nil {
				onDone()
			}
		})
	}()
}
