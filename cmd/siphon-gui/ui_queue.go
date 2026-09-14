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
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/hook"
	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

// refreshEvery, listenin en sık ne kadar yeniden çizileceği. Olay başına
// çizmek 20 paralel indirmede saniyede yüzlerce yenileme demekti; burada
// değişiklik varsa çiziliyor, yoksa hiç.
const refreshEvery = 150 * time.Millisecond

const (
	prefOutDir     = "out_dir"
	prefSpeedLimit = "speed_limit_mbps"
	prefSegments   = "segments"
	prefQuotaCmd   = "quota_command"
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
	quotaCmd *widget.Entry
	segments *widget.Select

	banner     *fyne.Container
	bannerText *widget.Label
	addBtn     *widget.Button
	pauseAll   *widget.Button
	list       *widget.List
	status     *widget.Label
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
			q.vm.Notify(err.Error())
			return
		}
		eng.SetSpeedLimit(bps)
		prefs.SetFloat(prefSpeedLimit, float64(bps)/(1024*1024))
	}
	speedBox := container.NewHBox(widget.NewLabel("Hız sınırı"), container.NewGridWrap(fyne.NewSize(70, 36), q.speed), widget.NewLabel("MB/s"))

	// Bağlantı/dosya: parçalı indirme. Site tavanı (max_segments) bunu kırpar;
	// pixeldrain ve mega'da 1, bunkr'da 4. Kullanıcı seçimi "istek", tavan
	// "izin". Etkin değer siteye göre değişir, bu yüzden etiket öyle diyor.
	q.segments = widget.NewSelect([]string{"1", "2", "4", "6", "8"}, func(v string) {
		n := 1
		fmt.Sscanf(v, "%d", &n)
		eng.SetSegments(n)
		prefs.SetInt(prefSegments, n)
	})
	if n := prefs.IntWithFallback(prefSegments, queue.DefaultSegments); n > 0 {
		q.segments.SetSelected(fmt.Sprint(n))
	}
	segBox := container.NewHBox(widget.NewLabel("Bağlantı/dosya"), q.segments)

	toolbar := container.NewHBox(q.pauseAll, clearBtn, openBtn, widget.NewLabel("   "), speedBox, widget.NewLabel("  "), segBox)

	// --- VPN değiştirme komutu (isteğe bağlı) ---
	// mega'nın IP başına kotası dolunca çalıştırılır; VPN sunucusunu
	// değiştiren bir komut (MegaBasterd'in "509'da komut çalıştır" özelliği).
	// BOŞ OLMASI NORMAL: o zaman kota dolunca sistem bildirimi gelir,
	// kullanıcı VPN'i kendi programından değiştirir, kuyruk 30 sn içinde
	// fark edip sürer. Kutu yalnızca bu adımı otomatikleştirmek isteyene.
	q.quotaCmd = widget.NewEntry()
	q.quotaCmd.SetPlaceHolder("isteğe bağlı — boşsa kota dolunca bildirim gelir, VPN'i sen değiştirirsin")
	if saved := prefs.String(prefQuotaCmd); saved != "" {
		q.quotaCmd.SetText(saved)
		eng.SetQuotaCommand(saved)
	}
	q.quotaCmd.OnChanged = func(v string) {
		eng.SetQuotaCommand(v)
		prefs.SetString(prefQuotaCmd, strings.TrimSpace(v))
	}
	tryBtn := widget.NewButton("Dene", q.tryQuotaCommand)
	quotaRow := container.NewBorder(nil, nil, widget.NewLabel("VPN değiştirme komutu"), tryBtn, q.quotaCmd)
	// Ana ekranda değil: çoğu kullanıcının (komut satırı olmayan VPN'ler,
	// ör. Kaspersky) işine yaramıyor ve kafa karıştırıyordu (ölçüldü).
	advanced := widget.NewAccordion(widget.NewAccordionItem("Gelişmiş", quotaRow))

	// --- Kota şeridi ---
	// Kota bekleyen iş varken listenin üstünde durur; bildirim ayarından
	// bağımsız, pencere açılınca ilk görülen şey. "Şimdi dene" VPN'i
	// değiştirmiş kullanıcının 30 sn'lik yoklamayı beklememesi için.
	q.bannerText = widget.NewLabel("")
	q.bannerText.Wrapping = fyne.TextWrapWord
	q.bannerText.TextStyle = fyne.TextStyle{Bold: true}
	retryBtn := widget.NewButton("Şimdi dene", func() {
		eng.RetryWaiting()
		vm.Replace(eng.Jobs())
	})
	retryBtn.Importance = widget.HighImportance
	q.banner = container.NewBorder(nil, nil, widget.NewIcon(theme.WarningIcon()), retryBtn, q.bannerText)
	q.banner.Hide()

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

	q.status = widget.NewLabel(vm.StatusLine())
	q.status.Wrapping = fyne.TextWrapWord

	top := container.NewVBox(
		container.NewBorder(nil, nil, widget.NewLabel("Linkler"), q.addBtn, linkBox),
		container.NewBorder(nil, nil, widget.NewLabel("Klasör"), pick, q.outDir),
		toolbar,
		advanced,
		q.banner,
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
		summary := q.vm.StatusLine()
		paused := q.eng.Paused()
		captcha := q.eng.PausedByCaptcha()
		banner := q.vm.QuotaBanner()
		fyne.Do(func() {
			q.list.Refresh()
			q.status.SetText(summary)
			if banner == "" {
				q.banner.Hide()
			} else {
				q.bannerText.SetText(banner)
				q.banner.Show()
			}
			switch {
			case captcha:
				// Sebep görünür olmalı: captcha kuyruğu durdurdu, kullanıcı
				// bir işe ▶ deyince ya da buraya basınca devam eder.
				q.pauseAll.SetText("▶ Captcha yüzünden duraklatıldı — sürdür")
			case paused:
				q.pauseAll.SetText("▶ Tümünü sürdür")
			default:
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
	q.vm.Notify(fmt.Sprintf("%d link çözümleniyor...", len(urls)))

	go func() {
		var added int
		var errs, failed []string
		for _, u := range urls {
			n, err := q.eng.Add(context.Background(), u, outDir)
			added += n
			switch {
			case err != nil:
				errs = append(errs, firstLine(err.Error()))
				failed = append(failed, u)
			case n == 0:
				// Çözümleme hatasız bitti ama dosya çıkmadı: boş klasör ya da
				// tüm dosyalar atlandı. "0 dosya eklendi" sebep söylemiyor.
				errs = append(errs, "indirilecek dosya bulunamadı: "+u)
				failed = append(failed, u)
			}
		}
		q.vm.Replace(q.eng.Jobs())
		q.vm.Notify(addSummary(added, errs))
		fyne.Do(func() {
			q.addBtn.Enable()
			// Eklenemeyen linkler kutuda kalır; kullanıcı düzeltip yeniden
			// dener, kopyalamak zorunda kalmaz. Eklenenler temizlenir.
			q.links.SetText(strings.Join(failed, "\n"))
		})
	}()
}

// addSummary, Ekle sonucunun tek satırı.
func addSummary(added int, errs []string) string {
	switch {
	case len(errs) > 0 && added == 0:
		return "Eklenemedi: " + strings.Join(errs, " | ")
	case len(errs) > 0:
		return fmt.Sprintf("%d dosya eklendi; %d link eklenemedi: %s", added, len(errs), strings.Join(errs, " | "))
	default:
		return fmt.Sprintf("%d dosya kuyruğa eklendi.", added)
	}
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

// tryQuotaCommand, kutudaki komutu şimdi çalıştırıp sonucunu gösterir:
// kullanıcı kota dolmasını beklemeden komutun doğru olduğunu görsün.
func (q *queueTab) tryQuotaCommand() {
	line := strings.TrimSpace(q.quotaCmd.Text)
	if line == "" {
		dialog.ShowInformation("Komut yok",
			"Bu kutu isteğe bağlı. Boş bırakırsan kota dolunca bir bildirim gelir; VPN'i kendi programından değiştirirsin, indirmeler kendiliğinden sürer.\n\n"+
				"Doldurursan Siphon kota dolunca bu komutu senin yerine çalıştırır. Hangi VPN'i kullandığına göre değişir; ör. NordVPN: \"C:\\Program Files\\NordVPN\\NordVPN.exe\" -c",
			q.win)
		return
	}
	q.vm.Notify("Komut deneniyor: " + line)
	go func() {
		start := time.Now()
		out, err := hook.Run(context.Background(), line, 0)
		took := time.Since(start).Round(time.Second)
		fyne.Do(func() {
			switch {
			case err != nil && out != "":
				dialog.ShowError(fmt.Errorf("komut başarısız (%v):\n\n%s", err, out), q.win)
				q.vm.Notify("Komut başarısız: " + firstLine(err.Error()))
			case err != nil:
				dialog.ShowError(fmt.Errorf("komut başarısız: %v", err), q.win)
				q.vm.Notify("Komut başarısız: " + firstLine(err.Error()))
			default:
				msg := fmt.Sprintf("Komut %s içinde bitti.", took)
				if out != "" {
					msg += "\n\nÇıktı:\n" + out
				}
				dialog.ShowInformation("Komut çalıştı", msg, q.win)
				q.vm.Notify(fmt.Sprintf("Komut çalıştı (%s).", took))
			}
		})
	}()
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
			q.vm.Notify(reason)
		}
		return
	}
	if err := openInExplorer(target, sel); err != nil {
		q.vm.Notify("Klasör açılamadı: " + err.Error())
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
