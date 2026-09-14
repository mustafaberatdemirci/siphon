package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// viewModel, kuyruğun ekranda gösterilecek hali. Motorun olayları buraya
// akar; arayüz buradan okur. İkisi arasında tek yön: model arayüzü bilmez.
//
// Neden ayrı: Fyne widget'ları test edilmesi zahmetli nesneler. Satır
// metinleri, yüzde, hız ve özet gibi kararlar burada saf fonksiyonlarda
// duruyor ve pencere olmadan test ediliyor.
type viewModel struct {
	mu    sync.Mutex
	order []string
	jobs  map[string]queue.Job
	speed map[string]*speedo
	rate  map[string]float64
	dirty bool

	// notice, bir eylemin sonucu ("Eklenemedi: …", "Klasör açılamadı: …").
	// Durum satırına doğrudan yazılmıyor; StatusLine onu özetle birleştiriyor.
	notice   string
	noticeAt time.Time
}

// noticeTTL: kuyruk doluyken bildirim bu kadar sonra özete yerini bırakır.
// Kuyruk boşken gösterecek başka şey yok, bildirim kalır.
const noticeTTL = 30 * time.Second

func newViewModel() *viewModel {
	return &viewModel{
		jobs:  map[string]queue.Job{},
		speed: map[string]*speedo{},
		rate:  map[string]float64{},
	}
}

// Apply, motordan gelen tek bir iş güncellemesini işler.
func (vm *viewModel) Apply(j queue.Job) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.apply(j, time.Now())
}

func (vm *viewModel) apply(j queue.Job, now time.Time) {
	if _, known := vm.jobs[j.ID]; !known {
		vm.order = append(vm.order, j.ID)
	}
	vm.jobs[j.ID] = j
	switch j.State {
	case queue.StateRunning:
		sp := vm.speed[j.ID]
		if sp == nil {
			sp = &speedo{}
			vm.speed[j.ID] = sp
		}
		vm.rate[j.ID] = sp.update(j.Done, now)
	default:
		// Duran işin hızı yok; "0 B/s" yazmak yerine hiç yazılmaz.
		delete(vm.speed, j.ID)
		delete(vm.rate, j.ID)
	}
	vm.dirty = true
}

// Replace, listeyi motorun tam kopyasıyla eşitler. Kaldırma ve temizleme
// olay üretmediği için bunlardan sonra çağrılır.
func (vm *viewModel) Replace(jobs []queue.Job) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	seen := map[string]bool{}
	vm.order = vm.order[:0]
	for _, j := range jobs {
		seen[j.ID] = true
		vm.order = append(vm.order, j.ID)
		if old, ok := vm.jobs[j.ID]; ok && old.State == queue.StateRunning && j.State == queue.StateRunning {
			// Hız ölçümünü koru; sadece veriyi tazele.
			vm.jobs[j.ID] = j
			continue
		}
		vm.apply(j, time.Now())
	}
	for id := range vm.jobs {
		if !seen[id] {
			delete(vm.jobs, id)
			delete(vm.speed, id)
			delete(vm.rate, id)
		}
	}
	vm.dirty = true
}

// TakeDirty, "değişti" bayrağını okuyup sıfırlar. Arayüz bunu periyodik
// olarak sorup yalnızca değişiklik varsa çiziyor; olay başına çizmek, 20
// paralel indirmede saniyede yüzlerce yenileme demekti.
func (vm *viewModel) TakeDirty() bool {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	d := vm.dirty
	vm.dirty = false
	return d
}

// row, tek satırın çizim için gereken hali.
type row struct {
	Job  queue.Job
	Rate float64
}

func (vm *viewModel) Len() int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return len(vm.order)
}

func (vm *viewModel) Row(i int) (row, bool) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if i < 0 || i >= len(vm.order) {
		return row{}, false
	}
	id := vm.order[i]
	return row{Job: vm.jobs[id], Rate: vm.rate[id]}, true
}

// QuotaBanner, listenin üstündeki uyarı şeridinin metni; kota bekleyen iş
// yoksa "". Bildirimlere bağımlı olmayan, pencere açılınca göze çarpan
// tek yer burası.
func (vm *viewModel) QuotaBanner() string {
	return vm.quotaBanner(time.Now())
}

func (vm *viewModel) quotaBanner(now time.Time) string {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	var n int
	var siteName string
	var earliest time.Time
	for _, id := range vm.order {
		j := vm.jobs[id]
		if j.State != queue.StateWaiting {
			continue
		}
		n++
		siteName = j.Site
		if !j.RetryAt.IsZero() && (earliest.IsZero() || j.RetryAt.Before(earliest)) {
			earliest = j.RetryAt
		}
	}
	if n == 0 {
		return ""
	}
	when := ""
	if !earliest.IsZero() && earliest.After(now) {
		when = fmt.Sprintf("; değiştirmezsen %s'de (%s sonra) kendiliğinden denenecek",
			earliest.Local().Format("15:04"), site.FormatWait(earliest.Sub(now)))
	}
	return fmt.Sprintf("%s kotası doldu — %d dosya bekliyor. VPN'de konumu değiştir; değişince indirmeler kendiliğinden sürer%s.",
		siteName, n, when)
}

func (vm *viewModel) Rows() []row {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	out := make([]row, 0, len(vm.order))
	for _, id := range vm.order {
		out = append(out, row{Job: vm.jobs[id], Rate: vm.rate[id]})
	}
	return out
}

// Notify, bir eylemin sonucunu durum satırına koyar ve yeniden çizim ister.
//
// ÖLÇÜLDÜ: eylem "Eklenemedi: …" yazıp modeli değiştirince 150 ms sonra
// yenileme döngüsü satırı özetle ("Kuyruk boş…") eziyordu; kullanıcı mega
// klasörünün neden eklenmediğini hiç göremedi. Bildirim artık modelde
// duruyor ve her çizimde yeniden yazılıyor.
func (vm *viewModel) Notify(msg string) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.notice = msg
	vm.noticeAt = time.Now()
	vm.dirty = true
}

// StatusLine, alt durum satırının tamamı: bildirim (varsa ve tazeyse) ve özet.
func (vm *viewModel) StatusLine() string {
	return vm.statusLine(time.Now())
}

func (vm *viewModel) statusLine(now time.Time) string {
	summary := vm.Summary()
	vm.mu.Lock()
	notice, at, empty := vm.notice, vm.noticeAt, len(vm.order) == 0
	vm.mu.Unlock()
	if notice == "" {
		return summary
	}
	if empty {
		return notice
	}
	if now.Sub(at) > noticeTTL {
		return summary
	}
	return notice + "  ·  " + summary
}

// Summary, kuyruğun özeti: "2 aktif · 5 sırada · 12 bitti · 24.3 MB/s".
func (vm *viewModel) Summary() string {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if len(vm.order) == 0 {
		return "Kuyruk boş. Link yapıştırıp Ekle'ye bas."
	}
	var active, queued, waiting, paused, done, failed int
	var total float64
	for _, id := range vm.order {
		j := vm.jobs[id]
		switch j.State {
		case queue.StateRunning:
			active++
			total += vm.rate[id]
		case queue.StateQueued:
			queued++
		case queue.StatePaused, queue.StateStopped:
			paused++
		case queue.StateWaiting:
			waiting++
		case queue.StateDone, queue.StateSkipped:
			done++
		case queue.StateFailed:
			failed++
		}
	}
	var parts []string
	if active > 0 {
		parts = append(parts, fmt.Sprintf("%d aktif", active))
	}
	if queued > 0 {
		parts = append(parts, fmt.Sprintf("%d sırada", queued))
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d kota bekliyor", waiting))
	}
	if paused > 0 {
		parts = append(parts, fmt.Sprintf("%d duraklatıldı", paused))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d hata", failed))
	}
	if done > 0 {
		parts = append(parts, fmt.Sprintf("%d bitti", done))
	}
	if r := humanRate(total); r != "" {
		parts = append(parts, r)
	}
	return strings.Join(parts, "  ·  ")
}

// --- Satır biçimlendirme (saf) ---

// stateLabel, durumun Türkçe etiketi.
func stateLabel(s queue.State) string {
	switch s {
	case queue.StateQueued:
		return "sırada"
	case queue.StateRunning:
		return "indiriliyor"
	case queue.StatePaused:
		return "duraklatıldı"
	case queue.StateDone:
		return "bitti"
	case queue.StateFailed:
		return "hata"
	case queue.StateSkipped:
		return "zaten inmiş"
	case queue.StateStopped:
		return "durduruldu"
	case queue.StateWaiting:
		return "kota bekliyor"
	}
	return string(s)
}

// waitingMeta, kota bekleyen satır: ne zaman kendiliğinden deneneceği ve
// kullanıcının ne yapabileceği. Saat MUTLAK yazılıyor ("20:31'de"): liste
// yalnızca değişiklikte çizildiği için geri sayım donuk kalırdı.
func waitingMeta(j queue.Job, now time.Time) string {
	if j.RetryAt.IsZero() {
		return "kota doldu  ·  ▶ ile şimdi dene"
	}
	left := j.RetryAt.Sub(now)
	if left < time.Minute {
		return "kota doldu  ·  birazdan yeniden denenecek"
	}
	return fmt.Sprintf("kota doldu  ·  VPN değişince ya da %s'de kendiliğinden sürer (%s)  ·  ▶ şimdi dene",
		j.RetryAt.Local().Format("15:04"), site.FormatWait(left))
}

// quotaHoldMessage, kota bildiriminin gövdesi: ne oldu, ne yapılabilir.
func quotaHoldMessage(retryAt, now time.Time) string {
	if retryAt.IsZero() || retryAt.Before(now) {
		return "VPN sunucusunu değiştirirsen indirmeler kendiliğinden sürer."
	}
	return fmt.Sprintf("VPN sunucusunu değiştirirsen indirmeler kendiliğinden sürer; değiştirmezsen %s'de (%s sonra) yeniden denenecek.",
		retryAt.Local().Format("15:04"), site.FormatWait(retryAt.Sub(now)))
}

// rowMeta, satırın sağ üstündeki bilgi: duruma göre boyut/hız/kalan ya da hata.
func rowMeta(r row) string {
	j := r.Job
	switch j.State {
	case queue.StateRunning:
		var b strings.Builder
		if j.Size > 0 {
			fmt.Fprintf(&b, "%s / %s", humanBytes(j.Done), humanBytes(j.Size))
		} else {
			b.WriteString(humanBytes(j.Done))
		}
		if s := humanRate(r.Rate); s != "" {
			b.WriteString("  ·  " + s)
		}
		if j.Size > 0 {
			if eta := humanETA(j.Size-j.Done, r.Rate); eta != "" {
				b.WriteString("  ·  kalan " + eta)
			}
		}
		return b.String()
	case queue.StatePaused:
		if j.Size > 0 {
			return fmt.Sprintf("%s / %s  ·  duraklatıldı", humanBytes(j.Done), humanBytes(j.Size))
		}
		return "duraklatıldı"
	case queue.StateWaiting:
		return waitingMeta(j, time.Now())
	case queue.StateFailed, queue.StateStopped:
		msg := firstLine(j.Error)
		if len([]rune(msg)) > 70 {
			msg = string([]rune(msg)[:67]) + "..."
		}
		if msg == "" {
			return stateLabel(j.State)
		}
		return stateLabel(j.State) + ": " + msg
	case queue.StateDone, queue.StateSkipped:
		return humanBytes(j.Size) + "  ·  " + stateLabel(j.State)
	default:
		if j.Size > 0 {
			return humanBytes(j.Size) + "  ·  " + stateLabel(j.State)
		}
		return stateLabel(j.State)
	}
}

// rowProgress, 0..1 arası ilerleme; boyut bilinmiyorsa 0 (çubuk uydurmuyor).
func rowProgress(j queue.Job) float64 {
	switch j.State {
	case queue.StateDone, queue.StateSkipped:
		return 1
	}
	if j.Size <= 0 {
		return 0
	}
	p := float64(j.Done) / float64(j.Size)
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

// actionFor, satırdaki ana düğmenin etiketi ve ne yapacağı.
type rowAction int

const (
	actionNone rowAction = iota
	actionPause
	actionResume
)

func actionFor(s queue.State) (label string, act rowAction) {
	switch s {
	case queue.StateRunning, queue.StateQueued:
		return "⏸", actionPause
	case queue.StatePaused, queue.StateFailed, queue.StateStopped, queue.StateWaiting:
		return "▶", actionResume
	default:
		return "✓", actionNone
	}
}
