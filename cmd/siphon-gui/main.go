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
	"log"
	"path/filepath"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/run"
)

// appID is the key under which Fyne preferences (output folder, speed limit) are stored.
const appID = "io.github.mustafaberatdemirci.siphon"

func main() {
	a := app.NewWithID(appID)
	registerToastIdentity(appID)
	a.SetIcon(appIcon())
	w := a.NewWindow("Siphon " + version)
	w.Resize(fyne.NewSize(980, 720))

	statePath, err := queue.DefaultStatePath()
	if err != nil {
		log.Printf("could not find the queue file path, persistence is off: %v", err)
		statePath = ""
	}

	// One instance only (instance.go): closing the window only hides it, so
	// launching the exe again is an easy mistake. A second launch shows this
	// window and exits instead of driving the same queue twice.
	instanceDir := ""
	if statePath != "" {
		instanceDir = filepath.Dir(statePath)
	}
	releaseInstance, running := claimInstance(instanceDir, func() {
		fyne.Do(func() {
			w.Show()
			w.RequestFocus()
		})
	})
	if running {
		return
	}
	defer releaseInstance()

	vm := newViewModel()

	// The engine's log goes to stderr, not the window; the UI already shows
	// errors in the job row (the Error field) and in the status line.
	eng, err := queue.New(queue.Options{
		StatePath: statePath,
		// Read here, not in the tab: the engine starts dispatching before the tab
		// is built, and would start the default number of jobs first.
		MaxActive: a.Preferences().IntWithFallback(prefMaxActive, queue.DefaultMaxActive),
		Events: run.Events{
			Errorf: func(f string, a ...any) { log.Printf("ERROR "+f, a...) },
		},
		OnChange: vm.Apply,
		OnNotice: vm.Notify,
		// A system notification when the quota runs out: the user gets the
		// "switch the VPN" news even when not looking at the window; once they
		// switch, the queue goes on by itself (probing). If the command box is
		// filled it is automatic anyway.
		OnQuotaHold: func(siteName string, retryAt time.Time) {
			msg := quotaHoldMessage(retryAt, time.Now())
			vm.Notify(siteName + " quota exceeded — " + msg)
			// Three channels, because none is guaranteed on its own:
			// notifications may be turned off account-wide (measured), the
			// window may be in the background, the sound may be off.
			a.SendNotification(fyne.NewNotification("Siphon — "+siteName+" quota exceeded", msg))
			fyne.Do(func() { requestAttention(w) })
		},
	})
	if err != nil {
		// The config couldn't be loaded: open the window but say why.
		w.SetContent(container.NewCenter(widget.NewLabel("Could not start:\n" + err.Error())))
		w.ShowAndRun()
		return
	}
	vm.Replace(eng.Jobs())

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
		vm.Notify("Closing, saving unfinished jobs...")
		go func() {
			shutdown()
			fyne.Do(a.Quit)
		}()
	}
	// Closing the window hides it in the notification area; downloads go on.
	tr := setupTray(a, w, a.Preferences(), quit)

	_, queueView := newQueueTab(w, a.Preferences(), eng, vm, tr.setQuota)
	_, doctorView := newDoctorTab(w)
	w.SetContent(container.NewAppTabs(
		container.NewTabItem("Download", queueView),
		container.NewTabItem("Diagnose", doctorView),
	))

	if statePath == "" {
		dialog.ShowInformation("Persistence off",
			"No folder was found for the queue file; the list is limited to this session.", w)
	}
	w.ShowAndRun()
	shutdown()
}
