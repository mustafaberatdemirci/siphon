// siphon-gui is Siphon's windowed interface.
//
// Not a SINGLE line of download logic lives here: items are downloaded with
// internal/run.Worker (the same code as the command line) and the queue is
// driven by internal/queue. This file only turns what happens into screen
// output and wires the buttons to the engine.
//
// The model is IDM-style: add links whenever you want, they get queued,
// pause/resume/remove whichever you want; the list stays in place when the
// app is closed and reopened, and unfinished jobs continue where they left off.
package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/browser"
	"github.com/mustafaberatdemirci/siphon/internal/instance"
	"github.com/mustafaberatdemirci/siphon/internal/logfile"
	"github.com/mustafaberatdemirci/siphon/internal/netcheck"
	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/run"
)

// appID is the key under which Fyne preferences (output folder, speed limit) are stored.
const appID = "io.github.mustafaberatdemirci.siphon"

func main() {
	a := app.NewWithID(appID)
	// Before anything is built: every label is made in this language.
	setLanguage(language(a.Preferences().String(prefLanguage)))
	registerToastIdentity(appID)
	a.SetIcon(appIcon())
	w := a.NewWindow("Siphon " + version)
	w.Resize(fyne.NewSize(980, 720))

	statePath, err := queue.DefaultStatePath()
	if err != nil {
		log.Printf("could not find the queue file path, persistence is off: %v", err)
		statePath = ""
	}

	// One instance only (internal/instance): closing the window only hides
	// it, so launching the exe again is an easy mistake. A second launch
	// shows this window and exits instead of driving the same queue twice;
	// links it was started with, or that the browser hands over, open this
	// window's Add dialog.
	instanceDir := ""
	if statePath != "" {
		instanceDir = filepath.Dir(statePath)
	}
	startLinks := linkArgs(os.Args[1:])
	var q *queueTab // set below, before the window runs; links wait for it
	releaseInstance, running := instance.Claim(instanceDir, instance.Handlers{
		Show: func() {
			fyne.Do(func() {
				w.Show()
				w.RequestFocus()
			})
		},
		Add: func(links []string) {
			fyne.Do(func() {
				if q != nil {
					q.receiveLinks(links)
				}
			})
		},
	})
	if running {
		if len(startLinks) > 0 {
			_ = instance.Add(instanceDir, startLinks)
		}
		return
	}
	defer releaseInstance()

	// The engine's log goes to a file next to the queue file (and stderr),
	// not the window; the UI already shows errors in the job row (the Error
	// field) and in the status line. Help → Copy diagnostic info reads it.
	if statePath != "" {
		if l, err := logfile.Open(filepath.Join(filepath.Dir(statePath), "siphon.log"), 0); err == nil {
			appLog = l
			defer l.Close()
			log.SetFlags(0)
			log.SetOutput(io.MultiWriter(os.Stderr, logWriter{l}))
		} else {
			log.Printf("could not open the log file: %v", err)
		}
	}
	appLog.Printf("Siphon %s started (%s/%s)", version, runtime.GOOS, runtime.GOARCH)

	vm := newViewModel()

	eng, err := queue.New(queue.Options{
		StatePath: statePath,
		// Read here, not in the tab: the engine starts dispatching before the tab
		// is built, and would start the default number of jobs first.
		MaxActive: a.Preferences().IntWithFallback(prefMaxActive, queue.DefaultMaxActive),
		Events: run.Events{
			Errorf: func(f string, a ...any) { log.Printf("ERROR "+f, a...) },
			Infof:  func(f string, a ...any) { appLog.Printf(f, a...) },
			Debugf: func(f string, a ...any) { appLog.Printf("debug "+f, a...) },
		},
		OnChange: vm.Apply,
		OnNotice: vm.Notify,
		// After a network error: is the user's network blocking the site?
		CheckNetwork: netcheck.Check,
		// A system notification when the quota runs out: the user gets the
		// "switch the VPN" news even when not looking at the window; once they
		// switch, the queue goes on by itself (probing). If the command box is
		// filled it is automatic anyway.
		OnQuotaHold: func(siteName string, retryAt time.Time) {
			msg := quotaHoldMessage(retryAt, time.Now())
			vm.Notify(Tf("%s quota exceeded — %s", siteName, msg))
			// Three channels, because none is guaranteed on its own:
			// notifications may be turned off account-wide (measured), the
			// window may be in the background, the sound may be off.
			a.SendNotification(fyne.NewNotification(Tf("Siphon — %s quota exceeded", siteName), msg))
			fyne.Do(func() { requestAttention(w) })
		},
	})
	if err != nil {
		// The config couldn't be loaded: open the window but say why.
		w.SetContent(container.NewCenter(widget.NewLabel(T("Could not start:") + "\n" + err.Error())))
		w.ShowAndRun()
		return
	}
	vm.Replace(eng.Jobs())
	// Outside the download hours nothing may start, not even before the
	// window is built.
	eng.SetOffHours(hoursHold(a.Preferences(), time.Now()) != "")

	ctx, cancel := context.WithCancel(context.Background())
	engineDone := make(chan struct{})
	go func() {
		eng.Run(ctx)
		close(engineDone)
	}()

	// Quitting: running jobs are canceled, the downloader syncs the .part and
	// writes its state, they drop into the queue file as "queued" and the next
	// launch continues by itself. Without waiting for the engine to finish,
	// the final state might not reach the disk. It runs once, however the app
	// ends: Quit in the notification area, the first-close dialog, or Windows
	// logging off (Fyne then ends its loop and ShowAndRun returns).
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			cancel()
			select {
			case <-engineDone:
			case <-time.After(8 * time.Second):
				log.Printf("the engine did not shut down within 8 s, exiting anyway")
			}
		})
	}
	quit := func() {
		vm.Notify(T("Closing, saving unfinished jobs..."))
		go func() {
			shutdown()
			fyne.Do(a.Quit)
		}()
	}
	// Closing the window hides it in the notification area; downloads go on.
	tr := setupTray(a, w, a.Preferences(), quit)

	var queueView fyne.CanvasObject
	q, queueView = newQueueTab(w, a.Preferences(), eng, vm, tr.setQuota)
	// Diagnose opens in a window of its own, made when first asked for: the
	// main window is the queue's alone.
	var diag fyne.Window
	q.openDiagnose = func() {
		if diag != nil {
			diag.Show()
			diag.RequestFocus()
			return
		}
		diag = a.NewWindow(T("Siphon — Diagnose"))
		_, view := newDoctorTab(diag)
		diag.SetContent(view)
		diag.Resize(fyne.NewSize(900, 640))
		diag.SetOnClosed(func() { diag = nil })
		diag.Show()
	}
	// Restarting (for a new language) is quitting, then starting again once
	// this process has let go of the queue and the instance.
	restartAfter := false
	q.restart = func() {
		restartAfter = true
		quit()
	}
	q.quit = quit
	w.SetMainMenu(q.mainMenu(quit))
	w.SetContent(queueView)
	w.SetOnDropped(q.onDropped)
	q.applyHours(time.Now()) // before the first download starts, if outside the hours
	go q.hoursLoop()
	go q.updateLoop()

	if statePath == "" {
		showInfo(T("Persistence off"),
			T("No folder was found for the queue file; the list is limited to this session."), w)
	}
	if len(startLinks) > 0 {
		// Started with links (by the browser, or "siphon-gui <link>"): the
		// Add dialog opens with them once the window runs.
		go fyne.Do(func() { q.receiveLinks(startLinks) })
	}
	w.ShowAndRun()
	shutdown()
	if restartAfter {
		// The next Siphon claims the instance; this one must have let go.
		releaseInstance()
		if err := startAgain(); err != nil {
			log.Printf("could not start Siphon again: %v", err)
		}
	}
}

// startAgain starts this program anew. Without the arguments: links it was
// started with were handled already and must not open the Add dialog again.
func startAgain() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.Command(exe).Start()
}

// linkArgs are the links among the program's arguments (a browser or a
// shortcut starting it with links); anything else is ignored.
func linkArgs(args []string) []string {
	var out []string
	for _, a := range args {
		out = append(out, browser.ExtractLinks(a)...)
	}
	return out
}
