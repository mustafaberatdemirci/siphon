// Package queue, kalıcı bir indirme kuyruğu sürer: işler tek tek başlar,
// duraklar, devam eder; liste uygulama kapanıp açılınca yerinde durur.
//
// Neden ayrı bir paket: run paketi toplu koşu için doğru model (komut satırı:
// bir URL ver, bitene kadar bekle). Pencere ise IDM tarzı bir kuyruk istiyor:
// link ekle, sıraya girsin, istediğini duraklat. İki model tek bir item'ı
// indirirken AYNI kodu (run.Worker) kullanıyor; farklılaşan yalnızca
// zamanlama ve yaşam döngüsü.
package queue

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// State, bir işin yaşam döngüsündeki yeri.
type State string

const (
	StateQueued  State = "queued"  // sırada, başlamayı bekliyor
	StateRunning State = "running" // indiriliyor
	StatePaused  State = "paused"  // kullanıcı durdurdu; .part yerinde, devam edilebilir
	StateDone    State = "done"    // indi ve kaydedildi
	StateFailed  State = "failed"  // kalıcı hata; devam denenebilir
	StateSkipped State = "skipped" // kayıt zaten vardı, dosya yerinde
	StateStopped State = "stopped" // captcha: kullanıcı müdahalesi gerekiyor
	StateWaiting State = "waiting" // sitenin kotası doldu; RetryAt'te kendiliğinden denenecek
)

// Active, işin şu anda kaynak tüketip tüketmediği.
func (s State) Active() bool { return s == StateRunning }

// Finished, işin bir daha kendiliğinden başlamayacağı.
func (s State) Finished() bool { return s == StateDone || s == StateSkipped }

// Resumable, kullanıcının "devam" diyebileceği durumlar. Waiting de burada:
// kullanıcı IP değiştirdiyse sıfırlanma saatini beklemek istemez, ▶ der.
func (s State) Resumable() bool {
	return s == StatePaused || s == StateFailed || s == StateStopped || s == StateWaiting
}

// Job, kuyruktaki tek bir dosya. Diske JSON olarak yazılıyor; bu yüzden
// yalnızca yeniden kurulabilir bilgi taşıyor: indirme adresi ve gizli
// malzeme (mega anahtarı) BURADA DEĞİL, SourcePage'den yeniden çözülüyor.
// mega'da link anahtarı taşıdığı için SourcePage'in kendisi hassastır; kuyruk
// dosyası kullanıcının kendi config klasöründe duruyor.
type Job struct {
	ID         string `json:"id"`
	Site       string `json:"site"`
	SourcePage string `json:"source_page"`
	OutDir     string `json:"out_dir"`
	Dir        string `json:"dir"`
	// Filename, gösterim ve ad sahipliği için. İlk Plan'dan sonra indiricinin
	// atadığı NİHAİ ad ("(2)" eki dahil); uygulama kapanıp açılınca aynı adla
	// devam etmenin garantisi bu.
	Filename string `json:"filename"`
	Path     string `json:"path,omitempty"` // nihai yol (Plan / Download)
	Size     int64  `json:"size"`
	Index    int    `json:"index"`

	State      State     `json:"state"`
	Done       int64     `json:"done"` // son bilinen inen bayt
	Error      string    `json:"error,omitempty"`
	AddedAt    time.Time `json:"added_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// RetryAt, Waiting durumunda işin kendiliğinden kuyruğa döneceği an.
	// Kalıcı: uygulama kapanıp açılsa da bekleme yerinde kalır.
	RetryAt time.Time `json:"retry_at,omitempty"`
}

// jobID, işin kararlı kimliği: aynı dosya aynı klasöre ikinci kez eklenirse
// aynı kimliği alır ve kuyrukta çoğalmaz.
func jobID(outDir, sourcePage, dir, filename string) string {
	h := sha256.New()
	for _, part := range []string{outDir, sourcePage, dir, filename} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
