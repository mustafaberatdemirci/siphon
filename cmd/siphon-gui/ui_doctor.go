package main

import (
	"context"
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/doctor"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// doctorTab, "Teşhis" sekmesi: her site için yedi katman raporu.
type doctorTab struct {
	out *widget.Entry
	btn *widget.Button
}

func newDoctorTab() (*doctorTab, fyne.CanvasObject) {
	d := &doctorTab{}
	d.out = widget.NewMultiLineEntry()
	d.out.Wrapping = fyne.TextWrapOff
	d.out.SetText("Teşhis Et'e bas: her site için yedi katman kontrol edilir\n" +
		"(DNS, TLS, Challenge, Fetch, Parse, ItemPage, CDN).")

	d.btn = widget.NewButton("Teşhis Et", d.run)
	d.btn.Importance = widget.HighImportance

	return d, container.NewBorder(container.NewHBox(d.btn), nil, nil, nil, d.out)
}

func (d *doctorTab) run() {
	d.btn.Disable()
	d.out.SetText("Teşhis çalışıyor...")

	go func() {
		var out strings.Builder
		cfgs, resolvers, err := run.Setup(run.Events{}, "", nil, nil)
		if err != nil {
			fyne.Do(func() {
				d.out.SetText("Config hatası:\n" + err.Error())
				d.btn.Enable()
			})
			return
		}
		named := make([]doctor.Named, 0, len(resolvers))
		for i, r := range resolvers {
			named = append(named, doctor.Named{Name: cfgs[i].Name, Resolver: r})
		}
		reports := doctor.Run(context.Background(), named)
		worst := doctor.Format(&out, reports)
		fmt.Fprintf(&out, "\nSonuç: %s\n", worst)
		if worst == site.StatusWarn {
			out.WriteString("UYARI başarısızlık değildir: bilinmeyen bir CDN host'u\n" +
				"indirmeyi durdurmaz, sadece bildirilir.\n")
		}

		fyne.Do(func() {
			d.out.SetText(out.String())
			d.btn.Enable()
		})
	}()
}
