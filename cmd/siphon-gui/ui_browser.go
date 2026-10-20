package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/browser"
)

// The browser integration in Settings: the browsers start Siphon's command
// line program as the extension's native messaging host, so it must be
// shipped next to this one; installing writes the registration pointing at
// it.

// shippedDir is the folder of the Siphon download: next to this program, or
// above Siphon.app on macOS.
func shippedDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	if runtime.GOOS == "darwin" && strings.HasSuffix(dir, filepath.Join("Siphon.app", "Contents", "MacOS")) {
		dir = filepath.Dir(filepath.Dir(filepath.Dir(dir)))
	}
	return dir
}

// cliName is the command line program's file name.
func cliName() string {
	if runtime.GOOS == "windows" {
		return "siphon.exe"
	}
	return "siphon"
}

// hostPath is the command line program shipped with this one, "" if missing.
func hostPath() string {
	p := filepath.Join(shippedDir(), cliName())
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

// extensionDir is the folder of the extensions shipped with Siphon (one per
// browser kind, browser/pack.sh makes them), "" if missing. Started through a
// link (winget puts one on the PATH), it is next to where the link points.
func extensionDir() string {
	dirs := []string{shippedDir()}
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Lstat(exe); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				dirs = append(dirs, filepath.Dir(real))
			}
		}
	}
	for _, d := range dirs {
		p := filepath.Join(d, "browser")
		if _, err := os.Stat(filepath.Join(p, "chromium", "manifest.json")); err == nil {
			return p
		}
	}
	return ""
}

// registeredHost and installHost are browser.Registered and browser.Install;
// tests replace them.
var (
	registeredHost = browser.Registered
	installHost    = browser.Install
)

// followBrowserHost moves the browser integration to host, this copy of
// Siphon's command line program, when it was installed for a program that is
// gone: a package manager's update removed the old version's folder, or the
// download was moved. The browsers would only fail to start it.
func followBrowserHost(host string) {
	reg := registeredHost()
	if reg == "" || host == "" || strings.EqualFold(filepath.Clean(reg), filepath.Clean(host)) {
		return
	}
	if _, err := os.Stat(reg); err == nil || !os.IsNotExist(err) {
		return
	}
	if _, err := installHost(host); err != nil {
		log.Printf("browser integration: %s is gone, and moving it to %s failed: %v", reg, host, err)
		return
	}
	log.Printf("browser integration: %s is gone; moved to %s", reg, host)
}

// browserStatus says whether the browsers would reach this copy of Siphon.
func browserStatus(host string) string {
	reg := browser.Registered()
	switch {
	case reg == "":
		return T("Not installed")
	case host != "" && strings.EqualFold(filepath.Clean(reg), filepath.Clean(host)):
		return T("Installed")
	default:
		return Tf("Installed for another copy of Siphon: %s", reg)
	}
}

// browserSettings is the Settings row of the browser integration.
func (q *queueTab) browserSettings() fyne.CanvasObject {
	status := widget.NewLabel(browserStatus(hostPath()))
	status.Wrapping = fyne.TextWrapWord
	refresh := func() { status.SetText(browserStatus(hostPath())) }
	install := widget.NewButton(T("Install"), func() {
		q.installBrowser()
		refresh()
	})
	remove := widget.NewButton(T("Remove"), func() {
		if err := browser.Uninstall(); err != nil {
			showInfo(T("Browser integration"), Tf("Could not remove the browser integration: %s", err.Error()), q.win)
		}
		refresh()
	})
	return container.NewBorder(nil, nil, nil, container.NewHBox(install, remove), status)
}

// installBrowser registers the host and says how to load the extension.
func (q *queueTab) installBrowser() {
	host := hostPath()
	if host == "" {
		showInfo(T("Browser integration"),
			Tf("Siphon's command line program (%s) must be next to this one: the browser starts it.", cliName()), q.win)
		return
	}
	names, err := browser.Install(host)
	if err != nil {
		showInfo(T("Browser integration"), Tf("Could not install the browser integration: %s", err.Error()), q.win)
		return
	}
	ext := extensionDir()
	if ext == "" {
		ext = T("the browser folder of the Siphon download")
	}
	showInfo(T("Browser integration"), Tf("Installed for %s. Now load the Siphon extension.\n\n"+
		"Chrome, Edge, Brave: open the extensions page (chrome://extensions), turn on Developer mode, choose Load unpacked and pick this folder:\n%s\n\n"+
		"Firefox: open about:debugging, This Firefox, Load Temporary Add-on, and pick manifest.json in this folder (it stays until Firefox restarts):\n%s",
		strings.Join(names, ", "), filepath.Join(ext, "chromium"), filepath.Join(ext, "firefox")), q.win)
}
