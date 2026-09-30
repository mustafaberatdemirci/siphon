package main

import (
	"context"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/doctor"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// doctorTab is the Diagnose window's content: a seven-layer report for each
// site, and the external tools (found or not, and a way to install them).
type doctorTab struct {
	out   *widget.Entry
	btn   *widget.Button
	tools *widget.Label
}

func newDoctorTab(win fyne.Window) (*doctorTab, fyne.CanvasObject) {
	d := &doctorTab{}
	d.out = widget.NewMultiLineEntry()
	d.out.Wrapping = fyne.TextWrapOff
	d.out.SetText(T("Press Diagnose: seven layers are checked for each site\n" +
		"(DNS, TLS, Challenge, Fetch, Parse, ItemPage, CDN)."))

	d.btn = widget.NewButton(T("Diagnose"), d.run)
	d.btn.Importance = widget.HighImportance

	d.tools = widget.NewLabel(toolsStatus())
	d.tools.TextStyle = fyne.TextStyle{Monospace: true}
	install := widget.NewButton(T("Install tools…"), func() {
		showInstallTools(win, func() { d.tools.SetText(toolsStatus()) })
	})

	top := container.NewVBox(container.NewHBox(d.btn, install), d.tools)
	return d, container.NewBorder(top, nil, nil, nil, d.out)
}

func (d *doctorTab) run() {
	d.btn.Disable()
	d.out.SetText(T("Diagnosing..."))

	go func() {
		var out strings.Builder
		cfgs, resolvers, err := run.Setup(run.Events{}, "", nil, nil, nil)
		if err != nil {
			fyne.Do(func() {
				d.out.SetText(T("Configuration error:") + "\n" + err.Error())
				d.btn.Enable()
			})
			return
		}
		named := make([]doctor.Named, 0, len(resolvers))
		for i, r := range resolvers {
			if _, fallback := r.(site.Fallback); fallback {
				continue // plain file links: no site to diagnose
			}
			named = append(named, doctor.Named{Name: cfgs[i].Name, Resolver: r})
		}
		reports := doctor.Run(context.Background(), named)
		worst := doctor.Format(&out, reports)
		out.WriteString("\n" + Tf("Result: %s", worst) + "\n")
		if worst == site.StatusWarn {
			out.WriteString(T("WARN is not a failure: an unknown CDN host\n"+
				"doesn't stop the download, it is only reported.") + "\n")
		}

		fyne.Do(func() {
			d.out.SetText(out.String())
			d.btn.Enable()
		})
	}()
}
