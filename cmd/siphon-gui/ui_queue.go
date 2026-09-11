package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

// refreshEvery, listenin en sık ne kadar yeniden çizileceği. Olay başına
// çizmek 20 paralel indirmede saniyede yüzlerce yenileme demekti; burada
// değişiklik varsa çiziliyor, yoksa hiç.
const refreshEvery = 150 * time.Millisecond

const (
	prefOutDir     = "out_dir"
	prefSpeedLimit = "speed_limit_mbps"
)

// queueTab, "İndir" sekmesi: link ekleme, kuyruk listesi, satır başına
// kontroller, genel düğmeler ve durum satırı.
type queueTab struct {
	win   fyne.Window
	prefs fyne.Preferences
	eng   *queue.Engine
	vm    *viewModel

	links    *widget.Entry
	outDir   *widget.Entry
	speed    *widget.Entry
	addBtn   *widget.Button
	pauseAll *widget.Button
	list     *widget.List
	status   *widget.Label
}

func newQueueTab(win fyne.Window, prefs fyne.Preferences, eng *queue.Engine, vm *viewModel) (*queueTab, fyne.CanvasObject) {
	q := &queueTab{win: win, prefs: prefs, eng: eng, vm: vm}

	// --- Link girişi ---
	q.links = widget.NewMultiLineEntry()
	q.links.SetPlaceHolder("Linkleri yapıştır — satır başına bir tane.\n" +
		"pixeldrain.com/l/…   bunkr.ws/a/…   mega.nz/folder/…#…")
	q.links.Wrapping = fyne.TextWrapOff
	q.addBtn = widget.NewButton("Ekle", q.add)
	q.addBtn.Importance = widget.HighImportance
	linkBox := container.NewVScroll(q.links)
	linkBox.SetMinSize(fyne.NewSize(0, 84))

	// --- Klasör ---
	q.outDir = widget.NewEntry()
	if saved := prefs.String(prefOutDir); saved != "" {
		q.outDir.SetText(saved)
	} else {
		q.outDir.SetText(defaultOutDir())
	}
	q.outDir.OnChanged = func(s string) { prefs.SetString(prefOutDir, normalizeDir(s)) }
	pick := widget.NewButton("Seç...", func() {
		dialog.ShowFolderOpen(func(lu fyne.ListableURI, err error) {
			if err != nil || lu == nil {
				return
			}
			// Path() EĞİK ÇİZGİLİ geliyor; normalize edilmeden kutuya
			// yazılırsa "Klasörü aç" Belgeler'i açar. Ayrıntı normalizeDir'de.
			q.outDir.SetText(normalizeDir(lu.Path()))
		}, win)
	})

	// --- Araç çubuğu ---
	q.pauseAll = widget.NewButton("⏸ Tümünü duraklat", q.togglePauseAll)
	clearBtn := widget.NewButton("Bitenleri temizle", func() {
		eng.ClearFinished()
		vm.Replace(eng.Jobs())
	})
	openBtn := widget.NewButton("Klasörü aç", q.openFolder)

	q.speed = widget.NewEntry()
	q.speed.SetPlaceHolder("0")
	if v := prefs.Float(prefSpeedLimit); v > 0 {
		q.speed.SetText(strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), "."))
		eng.SetSpeedLimit(int64(v * 1024 * 1024))
	}
	q.speed.OnChanged = func(s string) {
		bps, err := parseSpeedLimit(s)
		if err != nil {
			q.status.SetText(err.Error())
			return
		}
		eng.SetSpeedLimit(bps)
		prefs.SetFloat(prefSpeedLimit, float64(bps)/(1024*1024))
	}
	speedBox := container.NewHBox(widget.NewLabel("Hız sınırı"), container.NewGridWrap(fyne.NewSize(70, 36), q.speed), widget.NewLabel("MB/s"))

	toolbar := container.NewHBox(q.pauseAll, clearBtn, openBtn, widget.NewLabel("   "), speedBox)

	// --- Liste ---
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

	q.status = widget.NewLabel(vm.Summary())
	q.status.Wrapping = fyne.TextWrapWord

	top := container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("Linkler"), q.addBtn, linkBox),
		container.NewBorder(nil, nil, widget.NewLabel("Klasör"), pick, q.outDir),
		toolbar,
	)
	root := container.NewBorder(top, q.status, nil, nil, q.list)

	go q.refreshLoop()
	return q, root
}

// refreshLoop, model değiştiyse listeyi ve durum satırını yeniden çizer.
func (q *queueTab) refreshLoop() {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for range t.C {
		if !q.vm.TakeDirty() {
			continue
		}
		summary := q.vm.Summary()
		paused := q.eng.Paused()
		fyne.Do(func() {
			q.list.Refresh()
			q.status.SetText(summary)
			if paused {
				q.pauseAll.SetText("▶ Tümünü sürdür")
			} else {
				q.pauseAll.SetText("⏸ Tümünü duraklat")
			}
		})
	}
}

// add, girilen linkleri arka planda çözüp kuyruğa ekler. Çözümleme ağ
// gerektirir; arayüzü kilitlememek için goroutine'de.
func (q *queueTab) add() {
	urls := parseLinks(q.links.Text)
	if len(urls) == 0 {
		dialog.ShowInformation("Link yok", "Önce en az bir link yapıştır. Satır başına bir link.", q.win)
		return
	}
	outDir := normalizeDir(q.outDir.Text)
	if outDir == "" {
		dialog.ShowInformation("Klasör yok", "Bir çıktı klasörü seç.", q.win)
		return
	}
	q.addBtn.Disable()
	q.status.SetText(fmt.Sprintf("%d link çözümleniyor...", len(urls)))
	q.links.SetText("")

	go func() {
		var added int
		var errs []string
		for _, u := range urls {
			n, err := q.eng.Add(context.Background(), u, outDir)
			added += n
			if err != nil {
				errs = append(errs, firstLine(err.Error()))
			}
		}
		q.vm.Replace(q.eng.Jobs())
		fyne.Do(func() {
			q.addBtn.Enable()
			switch {
			case len(errs) > 0 && added == 0:
				q.status.SetText("Eklenemedi: " + strings.Join(errs, " | "))
			case len(errs) > 0:
				q.status.SetText(fmt.Sprintf("%d dosya eklendi; %d link hatalı: %s", added, len(errs), strings.Join(errs, " | ")))
			default:
				q.status.SetText(fmt.Sprintf("%d dosya kuyruğa eklendi.", added))
			}
		})
	}()
}

func (q *queueTab) togglePauseAll() {
	if q.eng.Paused() {
		q.eng.ResumeAll()
		q.pauseAll.SetText("⏸ Tümünü duraklat")
	} else {
		q.eng.PauseAll()
		q.pauseAll.SetText("▶ Tümünü sürdür")
	}
	q.vm.Replace(q.eng.Jobs())
}

// openFolder, son inen dosyayı klasöründe seçili açar; yoksa çıktı kökünü.
func (q *queueTab) openFolder() {
	var lastPath, lastDir string
	for _, r := range q.vm.Rows() {
		j := r.Job
		switch j.State {
		case queue.StateDone, queue.StateSkipped:
			if j.Path != "" {
				lastPath = j.Path
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
			q.status.SetText(reason)
		}
		return
	}
	if err := openInExplorer(target, sel); err != nil {
		q.status.SetText("Klasör açılamadı: " + err.Error())
	}
}

// removeJob, satırdaki ✕: yarım dosya varsa ne yapılacağını sorar.
func (q *queueTab) removeJob(j queue.Job) {
	if j.State.Finished() || j.Done == 0 {
		q.eng.Remove(j.ID, !j.State.Finished())
		q.vm.Replace(q.eng.Jobs())
		return
	}
	d := dialog.NewConfirm("Kuyruktan kaldır",
		fmt.Sprintf("%s\n\n%s inmiş durumda. Yarım dosya silinsin mi?\n(Hayır: dosya kalır, sonra aynı linkle devam edilebilir.)",
			shortName(j.Filename), humanBytes(j.Done)),
		func(wipe bool) {
			q.eng.Remove(j.ID, wipe)
			q.vm.Replace(q.eng.Jobs())
		}, q.win)
	d.SetConfirmText("Sil")
	d.SetDismissText("Sakla")
	d.Show()
}

// ---------- Satır widget'ı ----------

// jobRow, listedeki tek satır: ad, bilgi, ilerleme çubuğu, iki düğme.
// Fyne satırları geri dönüştürdüğü için set() her çağrıda düğmelerin
// hedefini yeniden bağlıyor.
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
