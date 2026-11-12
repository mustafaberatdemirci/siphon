package main

// Unpacking finished archives (internal/extract via the queue): off by
// default; Settings turns it on, and separately whether the archive is
// deleted once unpacked.

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

const (
	prefUnpack       = "unpack"
	prefUnpackDelete = "unpack_delete"
)

// unpackSetting is the two checks; each change reaches the queue right away.
func (q *queueTab) unpackSetting() fyne.CanvasObject {
	prefs, eng := q.prefs, q.eng
	del := widget.NewCheck(T("Delete the archive after unpacking"), nil)
	on := widget.NewCheck(T("Unpack finished archives"), nil)
	apply := func() {
		prefs.SetBool(prefUnpack, on.Checked)
		prefs.SetBool(prefUnpackDelete, del.Checked)
		eng.SetUnpack(on.Checked, del.Checked)
		if on.Checked {
			del.Enable()
		} else {
			del.Disable()
		}
	}
	on.SetChecked(prefs.Bool(prefUnpack))
	del.SetChecked(prefs.Bool(prefUnpackDelete))
	on.OnChanged = func(bool) { apply() }
	del.OnChanged = func(bool) { apply() }
	apply()
	return container.NewVBox(on, del)
}
