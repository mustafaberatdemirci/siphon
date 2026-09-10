// siphon-gui, Siphon'un pencereli arayüzüdür.
//
// İndirme mantığının TEK satırı burada değil: her şey internal/run'da ve komut
// satırı sürümü de aynı hattı çağırıyor. Bu dosya yalnızca olan biteni ekrana
// çeviriyor. Arayüzün motoru kopyalaması, iki sürümün zamanla farklı davranması
// demek olurdu.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	"github.com/mustafaberatdemirci/siphon/internal/doctor"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

func main() {
	a := app.New()
	w := a.NewWindow("Siphon — pixeldrain & bunkr indirici")
	w.Resize(fyne.NewSize(920, 680))

	ui := newUI(w)
	w.SetContent(container.NewAppTabs(
		container.NewTabItem("İndir", ui.downloadTab()),
		container.NewTabItem("Teşhis", ui.doctorTab()),
	))

	// Pencere kapanırken süren indirme iptal edilir: indirici .part'ı sync
	// edip durumu yazar, yani yarım dosya bırakmaz ve sonraki koşu devam eder.
	w.SetOnClosed(ui.cancel)

	w.ShowAndRun()
}

type ui struct {
	win fyne.Window

	links    *widget.Entry
	outDir   *widget.Entry
	listOnly *widget.Check

	startBtn *widget.Button
	stopBtn  *widget.Button
	openBtn  *widget.Button

	status   binding.String
	progress binding.Float
	items    binding.StringList

	doctorOut *widget.Entry
	doctorBtn *widget.Button

	mu      sync.Mutex
	cancelF context.CancelFunc
	rows    map[string]int // SourcePage -> satır numarası
	total   int
	done    int

	// Hız ölçümü. perItem her dosyanın kendi hızını, overall koşunun toplam
	// hızını tutuyor. bytesByKey, her dosyanın son bilinen kümülatif baytı;
	// toplam bayt bunun üzerinden hesaplanıyor çünkü Progress kümülatif
	// değer gönderiyor, artış değil.
	perItem    map[string]*speedo
	bytesByKey map[string]int64
	totalBytes int64
	overall    *speedo
}

func newUI(w fyne.Window) *ui {
	u := &ui{
		win:        w,
		status:     binding.NewString(),
		progress:   binding.NewFloat(),
		items:      binding.NewStringList(),
		rows:       map[string]int{},
		perItem:    map[string]*speedo{},
		bytesByKey: map[string]int64{},
		overall:    &speedo{},
	}
	_ = u.status.Set("Hazır. Linkleri yapıştır ve İndir'e bas.")
	return u
}

// ---------- İndir sekmesi ----------

func (u *ui) downloadTab() fyne.CanvasObject {
	u.links = widget.NewMultiLineEntry()
	u.links.SetPlaceHolder("Linkleri buraya yapıştır — satır başına bir tane.\n" +
		"https://pixeldrain.com/l/...\nhttps://bunkr.ws/a/...\n\n# ile başlayan satırlar yorumdur.")
	u.links.Wrapping = fyne.TextWrapOff

	u.outDir = widget.NewEntry()
	u.outDir.SetText(defaultOutDir())
	pick := widget.NewButton("Seç...", func() {
		dialog.ShowFolderOpen(func(lu fyne.ListableURI, err error) {
			if err != nil || lu == nil {
				return
			}
			// Path() EĞİK ÇİZGİLİ geliyor; normalize edilmeden kutuya yazılırsa
			// "Klasörü aç" Belgeler'i açar. Ayrıntı normalizeDir'de.
			u.outDir.SetText(normalizeDir(lu.Path()))
		}, u.win)
	})

	u.listOnly = widget.NewCheck("Sadece listele (indirme)", nil)

	u.startBtn = widget.NewButton("İndir", u.start)
	u.startBtn.Importance = widget.HighImportance
	u.stopBtn = widget.NewButton("Durdur", u.cancel)
	u.stopBtn.Disable()
	u.openBtn = widget.NewButton("Klasörü aç", u.openOutDir)

	statusLabel := widget.NewLabelWithData(u.status)
	statusLabel.Wrapping = fyne.TextWrapWord
	bar := widget.NewProgressBarWithData(u.progress)

	list := widget.NewListWithData(u.items,
		func() fyne.CanvasObject {
			l := widget.NewLabel("")
			l.Truncation = fyne.TextTruncateEllipsis
			return l
		},
		func(di binding.DataItem, o fyne.CanvasObject) {
			s, ok := di.(binding.String)
			if !ok {
				return
			}
			txt, _ := s.Get()
			o.(*widget.Label).SetText(txt)
		})

	// Kaydırılabilir bir kap içinde: yükseklik sabit (150), genişlik pencereyle
	// birlikte büyüyor. GridWrap ile sabitlemek pencere büyütüldüğünde kutunun
	// dar kalmasına yol açıyordu.
	linkBox := container.NewVScroll(u.links)
	linkBox.SetMinSize(fyne.NewSize(0, 150))

	top := container.NewVBox(
		widget.NewLabel("Linkler"),
		linkBox,
		container.NewBorder(nil, nil, widget.NewLabel("Klasör"), pick, u.outDir),
		container.NewHBox(u.startBtn, u.stopBtn, u.openBtn, u.listOnly),
		statusLabel,
		bar,
	)
	return container.NewBorder(top, nil, nil, nil, list)
}

// normalizeDir, kullanıcıdan veya klasör seçiciden gelen yolu Windows'un
// beklediği biçime çevirir.
//
// ÖLÇÜLDÜ: Fyne'ın klasör seçicisi yolu URI'den türetiyor ve EĞİK ÇİZGİYLE
// veriyor — "E:\x" seçince kutuya "E:/x" yazılıyor. Go'nun dosya çağrıları
// eğik çizgiyi kabul ettiği için indirme doğru yere iniyor ve hata görünmez
// kalıyor; ama explorer.exe kabul etmiyor. Yolu tanımayınca hata da vermiyor,
// sessizce Belgeler klasörünü açıyor. Kullanıcının gördüğü davranış buydu.
//
// Clean ayrıca sondaki ayracı atıyor ve bu ikinci bir tuzağı kapatıyor:
// "E:\x\" komut satırında explorer "E:\x\" olarak tırnaklanır, sondaki ters
// çizgi kapanış tırnağını kaçırır ve explorer yine yolu tanımaz.
//
// Tırnaklar da soyuluyor: Windows'un "Yol olarak kopyala" komutu yolu
// tırnak içinde veriyor ve yapıştıran herkes bunu fark etmiyor.
func normalizeDir(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"`)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Çıplak sürücü harfi ("E:") SÜRÜCÜYE GÖRELİ bir yoldur, kökü değil:
	// Clean onu "E:." yapar, yani "E: sürücüsünün geçerli dizini". Bu, işlem
	// durumuna bağlı bir yer; klasör kutusuna "E:" yazan kimse bunu kastetmez.
	// Kök olarak yorumluyoruz.
	//
	// VolumeName girdinin TAMAMINA eşitse elde yalnızca sürücü harfi var
	// demektir ("E:"). "E:\", "E:/x" ve UNC yolları eşit olmaz, dokunulmaz.
	if s == filepath.VolumeName(s) {
		s += string(filepath.Separator)
	}
	return filepath.Clean(filepath.FromSlash(s))
}

// defaultOutDir, makul bir başlangıç klasörü seçer.
func defaultOutDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		d := filepath.Join(home, "Downloads")
		if fi, serr := os.Stat(d); serr == nil && fi.IsDir() {
			return filepath.Join(d, "siphon")
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func (u *ui) openOutDir() {
	dir := normalizeDir(u.outDir.Text)
	if dir == "" {
		return
	}
	// Klasör SESSİZCE OLUŞTURULMUYOR. Eskiden MkdirAll çağrılıyordu ve bu,
	// henüz hiçbir şey indirilmemişken düğmeye basıldığında boş bir klasör
	// yaratıp açıyordu: kullanıcıya "düğme bozuk" gibi görünen davranış.
	// Klasör yoksa sebebini söylemek daha dürüst.
	fi, err := os.Stat(dir)
	if err != nil {
		// YALNIZCA "yok" hatası "henüz oluşmadı" demektir. İzin reddi,
		// geçersiz sürücü veya erişilemeyen ağ payı için aynı cümleyi kurmak
		// doğrulanmamış bir şey iddia etmek olurdu.
		if errors.Is(err, fs.ErrNotExist) {
			_ = u.status.Set("Klasör henüz yok: " + dir + " — indirme başlayınca oluşacak.")
		} else {
			_ = u.status.Set("Klasöre erişilemedi: " + err.Error())
		}
		return
	}
	if !fi.IsDir() {
		_ = u.status.Set("Bu bir klasör değil: " + dir)
		return
	}

	// MUTLAK yola çevriliyor: göreli bir yol verilirse explorer onu KENDİ
	// çalışma dizinine göre çözer, bizimkine göre değil; yani yanlış klasörü
	// açar. Abs başarısız olursa elimizdekiyle devam etmek hiç denememekten iyi.
	if abs, aerr := filepath.Abs(dir); aerr == nil {
		dir = abs
	}

	// explorer TAM YOLLA çağrılıyor: çıplak ad %PATH% üzerinden çözülür ve
	// yazılabilir bir PATH dizinine konan explorer.exe bu düğmeyle çalışırdı.
	explorer := "explorer"
	if root := os.Getenv("SystemRoot"); root != "" {
		explorer = filepath.Join(root, "explorer.exe")
	}

	// explorer.exe BAŞARIDA BİLE 1 döndürüyor, bu yüzden çıkış kodu
	// kontrol edilmiyor; yalnızca başlatma hatası anlamlı.
	if err := exec.Command(explorer, dir).Start(); err != nil {
		_ = u.status.Set("Klasör açılamadı: " + err.Error())
	}
}

// ---------- Koşu ----------

func (u *ui) start() {
	urls := parseLinks(u.links.Text)
	if len(urls) == 0 {
		dialog.ShowInformation("Link yok",
			"Önce en az bir link yapıştır. Satır başına bir link.", u.win)
		return
	}
	outDir := normalizeDir(u.outDir.Text)
	if outDir == "" {
		dialog.ShowInformation("Klasör yok", "Bir çıktı klasörü seç.", u.win)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	u.mu.Lock()
	u.cancelF = cancel
	u.rows = map[string]int{}
	u.total = 0
	u.done = 0
	u.perItem = map[string]*speedo{}
	u.bytesByKey = map[string]int64{}
	u.totalBytes = 0
	u.overall = &speedo{}
	u.mu.Unlock()

	_ = u.items.Set(nil)
	_ = u.progress.Set(0)
	_ = u.status.Set(fmt.Sprintf("%d link çözümleniyor...", len(urls)))
	u.setRunning(true)

	listOnly := u.listOnly.Checked

	go func() {
		defer cancel()
		sum, err := run.Run(ctx, run.Options{
			URLs:        urls,
			OutDir:      outDir,
			ResolveOnly: listOnly,
		}, u.events())

		fyne.Do(func() {
			u.setRunning(false)
			if err != nil {
				// Kullanım/konfigürasyon hatası: koşu hiç başlamadı.
				_ = u.status.Set("Hata: " + err.Error())
				dialog.ShowError(err, u.win)
				return
			}
			_ = u.status.Set(summaryLine(sum, listOnly))
			if sum.ExitCode() == run.ExitOK && !listOnly {
				_ = u.progress.Set(1)
			}
		})
	}()
}

func (u *ui) cancel() {
	u.mu.Lock()
	c := u.cancelF
	u.cancelF = nil
	u.mu.Unlock()
	if c != nil {
		_ = u.status.Set("Durduruluyor... yarım dosyalar korunuyor, sonra devam edebilir.")
		c()
	}
}

func (u *ui) setRunning(running bool) {
	if running {
		u.startBtn.Disable()
		u.stopBtn.Enable()
	} else {
		u.startBtn.Enable()
		u.stopBtn.Disable()
	}
}

// summaryLine, özeti tek satırlık insan diline çevirir.
//
// Çıkış kodu sözleşmesi burada da korunuyor: WARN benzeri durumlar "tamam"
// demiyor, ama kısmi başarı da "başarısız" demiyor.
func summaryLine(s run.Summary, listOnly bool) string {
	if listOnly {
		return fmt.Sprintf("Listeleme bitti: %d link, %d dosya bulundu.", s.URLs, s.Items)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Bitti: %d/%d dosya indi", s.Done, s.Items)
	if s.Skipped > 0 {
		fmt.Fprintf(&b, ", %d zaten vardı", s.Skipped)
	}
	if s.Failed > 0 {
		fmt.Fprintf(&b, ", %d başarısız", s.Failed)
	}
	if s.SkippedURLs > 0 {
		fmt.Fprintf(&b, ", %d link tanınmadı", s.SkippedURLs)
	}
	if s.Degraded > 0 {
		fmt.Fprintf(&b, ", %d dosya indi ama kaydı yazılamadı", s.Degraded)
	}
	if s.Halted {
		b.WriteString(" — koşu durduruldu")
	}
	return b.String()
}

// events, run paketinin olaylarını arayüze bağlar.
//
// Tüm geri çağrılar ARKA PLAN goroutine'lerinden geliyor. Veri bağlamaları
// (binding) bunun için güvenli; doğrudan widget değiştiren her şey fyne.Do
// içine alınıyor.
func (u *ui) events() run.Events {
	return run.Events{
		Errorf: func(f string, a ...any) {
			u.appendLine("  ! " + fmt.Sprintf(f, a...))
		},
		// ItemQueued, item çözülür çözülmez geliyor. Toplamı BURADAN saymak
		// zorundayız: URLResolved bir URL'in tüm item'ları bitince tetikleniyor,
		// yani onunla sayarsak ilerleme çubuğu koşu boyunca sıfırda kalır.
		ItemQueued: func(it site.Item) {
			u.mu.Lock()
			u.total++
			u.mu.Unlock()
			u.setRow(it, "⏳ "+shortName(it.Filename)+"  sırada")
			u.refreshStatus()
		},
		ItemResolved: func(it site.Item) {
			// Sadece listele modu. Toplamı BURADAN da saymak zorundayız:
			// ItemQueued yalnızca indirme yolunda tetikleniyor, yani
			// listelerken durum satırı baştan sona "çözümleniyor..." kalırdı.
			u.mu.Lock()
			u.total++
			u.done++
			u.mu.Unlock()
			u.appendLine(it.URL)
			u.refreshStatus()
		},
		ItemStarted: func(it site.Item) {
			u.setRow(it, "… "+shortName(it.Filename)+" başlıyor")
		},
		Progress: func(it site.Item, done, total int64) {
			rate := u.trackBytes(it, done)
			u.setRow(it, progressLine(it, done, total, rate))
			u.refreshStatus()
		},
		ItemDone: func(it site.Item, res dl.Result) {
			u.setRow(it, "✓ "+filepath.Base(res.Path)+"  ("+humanBytes(res.Size)+")")
			u.bump()
		},
		ItemFailed: func(it site.Item, err error) {
			u.setRow(it, "✗ "+shortName(it.Filename)+"  — "+firstLine(err.Error()))
			u.bump()
		},
		ItemSkipped: func(it site.Item, e store.Entry) {
			u.setRow(it, "• "+shortName(e.Filename)+"  (zaten indirilmiş)")
			u.bump()
		},
	}
}

func (u *ui) bump() {
	u.mu.Lock()
	u.done++
	done, total := u.done, u.total
	u.mu.Unlock()
	if total > 0 {
		_ = u.progress.Set(float64(done) / float64(total))
	}
	u.refreshStatus()
}

// trackBytes, bir item'ın kümülatif bayt sayısını işler ve o item'ın hızını
// döndürür. Toplam bayt ve genel hız da burada güncelleniyor.
//
// Progress KÜMÜLATİF değer gönderiyor (artış değil), bu yüzden toplamı bulmak
// için her item'ın son değerini saklayıp farkı almak gerekiyor.
func (u *ui) trackBytes(it site.Item, done int64) float64 {
	key := rowKey(it)
	now := time.Now()

	u.mu.Lock()
	prev := u.bytesByKey[key]
	delta := done - prev
	if delta < 0 {
		// İndirme baştan başlamış; toplamı geriye almak yerine bu item'ın
		// katkısını sıfırlayıp yeniden sayıyoruz.
		u.totalBytes -= prev
		delta = done
	}
	u.bytesByKey[key] = done
	u.totalBytes += delta
	totalBytes := u.totalBytes

	sp, ok := u.perItem[key]
	if !ok {
		sp = &speedo{}
		u.perItem[key] = sp
	}
	overall := u.overall
	u.mu.Unlock()

	overall.update(totalBytes, now)
	return sp.update(done, now)
}

// refreshStatus, durum satırını günceller: kaç dosya bitti, toplam ne kadar
// indi, genel hız ne.
func (u *ui) refreshStatus() {
	u.mu.Lock()
	done, total, bytes := u.done, u.total, u.totalBytes
	overall := u.overall
	u.mu.Unlock()

	var b strings.Builder
	if total > 0 {
		fmt.Fprintf(&b, "%d/%d dosya", done, total)
	} else {
		b.WriteString("çözümleniyor...")
	}
	if bytes > 0 {
		fmt.Fprintf(&b, "  ·  %s indirildi", humanBytes(bytes))
	}
	if r := humanRate(overall.rate()); r != "" {
		fmt.Fprintf(&b, "  ·  %s", r)
	}
	_ = u.status.Set(b.String())
}

// rowKey, bir item'ın tekil anahtarı. SourcePage tekil; bazı medya yollarında
// boş olabildiği için URL yedek.
func rowKey(it site.Item) string {
	if it.SourcePage != "" {
		return it.SourcePage
	}
	return it.URL
}

// setRow, bir item'ın satırını günceller veya ekler.
// Anahtar SourcePage: Index albümler arasında tekrar ediyor, SourcePage tekil.
func (u *ui) setRow(it site.Item, line string) {
	key := rowKey(it)

	// Length() ile Append() AYNI kilit altında olmak zorunda. İkisinin arasında
	// kilidi bırakmak, iki goroutine'in aynı uzunluğu okuyup aynı satırı
	// sahiplenmesi demek; sonuç, iki dosyanın tek satırı ezmesi. Bu yarışı
	// TestSetRowIsConcurrentSafe yakaladı.
	u.mu.Lock()
	defer u.mu.Unlock()

	if row, ok := u.rows[key]; ok {
		_ = u.items.SetValue(row, line)
		return
	}
	row := u.items.Length()
	u.rows[key] = row
	_ = u.items.Append(line)
}

func (u *ui) appendLine(line string) { _ = u.items.Append(line) }

// ---------- Teşhis sekmesi ----------

func (u *ui) doctorTab() fyne.CanvasObject {
	u.doctorOut = widget.NewMultiLineEntry()
	u.doctorOut.Wrapping = fyne.TextWrapOff
	u.doctorOut.SetText("Teşhis Et'e bas: her site için yedi katman kontrol edilir\n" +
		"(DNS, TLS, Challenge, Fetch, Parse, ItemPage, CDN).")

	u.doctorBtn = widget.NewButton("Teşhis Et", u.runDoctor)
	u.doctorBtn.Importance = widget.HighImportance

	return container.NewBorder(
		container.NewHBox(u.doctorBtn),
		nil, nil, nil,
		u.doctorOut,
	)
}

func (u *ui) runDoctor() {
	u.doctorBtn.Disable()
	u.doctorOut.SetText("Teşhis çalışıyor...")

	go func() {
		var out strings.Builder
		cfgs, resolvers, err := run.Setup(run.Events{}, "", nil, nil)
		if err != nil {
			fyne.Do(func() {
				u.doctorOut.SetText("Config hatası:\n" + err.Error())
				u.doctorBtn.Enable()
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
			u.doctorOut.SetText(out.String())
			u.doctorBtn.Enable()
		})
	}()
}

// ---------- Yardımcılar ----------

// parseLinks, metin alanını URL listesine çevirir.
// Komut satırındaki -i dosyası ile AYNI kurallar: boş satır ve # atlanır.
func parseLinks(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func progressLine(it site.Item, done, total int64, rate float64) string {
	name := shortName(it.Filename)

	var b strings.Builder
	if total <= 0 {
		// Boyut bilinmiyor (Content-Length yok ve resolver da bildirmemiş).
		// Yüzde uydurmak yerine ineni söylüyoruz.
		fmt.Fprintf(&b, "↓ %s  %s", name, humanBytes(done))
	} else {
		pct := int(float64(done) / float64(total) * 100)
		fmt.Fprintf(&b, "↓ %s  %%%d  (%s / %s)", name, pct, humanBytes(done), humanBytes(total))
	}
	if r := humanRate(rate); r != "" {
		fmt.Fprintf(&b, "  %s", r)
	}
	if total > 0 {
		if eta := humanETA(total-done, rate); eta != "" {
			fmt.Fprintf(&b, "  kalan %s", eta)
		}
	}
	return b.String()
}

func shortName(s string) string {
	s = filepath.Base(s)
	const max = 60
	if len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-3]) + "..."
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func humanBytes(n int64) string {
	if n < 0 {
		return "? B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/unit)
}
