// siphon-gui, Siphon'un pencereli arayüzüdür.
//
// İndirme mantığının TEK satırı burada değil: item'lar internal/run.Worker
// ile iniyor (komut satırıyla aynı kod), kuyruğu internal/queue sürüyor. Bu
// dosya yalnızca olan biteni ekrana çeviriyor ve düğmeleri motora bağlıyor.
//
// Model IDM tarzı: linkleri istediğin zaman ekle, sıraya girsin, istediğini
// duraklat/sürdür/kaldır; liste uygulama kapanıp açılınca yerinde dursun ve
// yarım kalanlar kaldığı yerden devam etsin.
package main

import (
	"context"
	"log"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/run"
)

// appID, Fyne tercihlerinin (çıktı klasörü, hız sınırı) saklandığı anahtar.
const appID = "io.github.mustafaberatdemirci.siphon"

func main() {
	a := app.NewWithID(appID)
	w := a.NewWindow("Siphon — pixeldrain, bunkr & mega indirici")
	w.Resize(fyne.NewSize(980, 720))

	vm := newViewModel()
	statePath, err := queue.DefaultStatePath()
	if err != nil {
		log.Printf("kuyruk dosyası yolu bulunamadı, kalıcılık kapalı: %v", err)
		statePath = ""
	}

	// Motorun log'u pencereye değil stderr'e gidiyor; arayüz hataları iş
	// satırında (Error alanı) ve durum satırında zaten gösteriyor.
	eng, err := queue.New(queue.Options{
		StatePath: statePath,
		MaxActive: queue.DefaultMaxActive,
		Events: run.Events{
			Errorf: func(f string, a ...any) { log.Printf("HATA "+f, a...) },
		},
		OnChange: vm.Apply,
	})
	if err != nil {
		// Config yüklenemedi: pencere açılsın ama sebebini söylesin.
		w.SetContent(container.NewCenter(widget.NewLabel("Başlatılamadı:\n" + err.Error())))
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

	_, queueView := newQueueTab(w, a.Preferences(), eng, vm)
	_, doctorView := newDoctorTab()
	w.SetContent(container.NewAppTabs(
		container.NewTabItem("İndir", queueView),
		container.NewTabItem("Teşhis", doctorView),
	))

	// Kapanış: çalışan işler iptal edilir, indirici .part'ı sync edip durumu
	// yazar, kuyruk dosyasına "queued" olarak düşerler ve bir sonraki açılış
	// kendiliğinden devam eder. Motorun bitmesi beklenmezse son durum diske
	// yazılamayabilirdi.
	w.SetCloseIntercept(func() {
		vm.Notify("Kapatılıyor, yarım işler kaydediliyor...")
		cancel()
		go func() {
			select {
			case <-engineDone:
			case <-time.After(8 * time.Second):
				log.Printf("motor 8 sn'de kapanmadı, pencere yine de kapatılıyor")
			}
			fyne.Do(w.Close)
		}()
	})

	if statePath == "" {
		dialog.ShowInformation("Kalıcılık kapalı",
			"Kuyruk dosyası için bir klasör bulunamadı; liste bu oturumla sınırlı.", w)
	}
	w.ShowAndRun()
}
