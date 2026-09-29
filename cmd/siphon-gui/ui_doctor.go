package main

import (
	"context"
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/doctor"
	"github.com/mustafaberatdemirci/siphon/internal/external"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// doctorTab is the "Diagnose" tab: a seven-layer report for each site.
type doctorTab struct {
	out *widget.Entry
	btn *widget.Button
}

func newDoctorTab() (*doctorTab, fyne.CanvasObject) {
	d := &doctorTab{}
	d.out = widget.NewMultiLineEntry()
	d.out.Wrapping = fyne.TextWrapOff
	d.out.SetText("Press Diagnose: seven layers are checked for each site\n" +
		"(DNS, TLS, Challenge, Fetch, Parse, ItemPage, CDN).")

	d.btn = widget.NewButton("Diagnose", d.run)
	d.btn.Importance = widget.HighImportance

	return d, container.NewBorder(container.NewHBox(d.btn), nil, nil, nil, d.out)
}

func (d *doctorTab) run() {
	d.btn.Disable()
	d.out.SetText("Diagnosing...")

	go func() {
		var out strings.Builder
		cfgs, resolvers, err := run.Setup(run.Events{}, "", nil, nil, nil)
		if err != nil {
			fyne.Do(func() {
				d.out.SetText("Configuration error:\n" + err.Error())
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
		var extra map[string]string
		for _, c := range cfgs {
			if c.Name == site.DirectName {
				extra = c.Extra
			}
		}
		out.WriteString("\ntools (for pages of sites Siphon doesn't know)\n")
		for _, l := range external.Find(extra).Lines() {
			out.WriteString(l + "\n")
		}
		fmt.Fprintf(&out, "\nResult: %s\n", worst)
		if worst == site.StatusWarn {
			out.WriteString("WARN is not a failure: an unknown CDN host\n" +
				"doesn't stop the download, it is only reported.\n")
		}

		fyne.Do(func() {
			d.out.SetText(out.String())
			d.btn.Enable()
		})
	}()
}
