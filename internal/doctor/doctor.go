// Package doctor, katman teşhisini çalıştırır ve okunabilir bir rapor basar.
//
// Bu paket aracın var oluş gerekçesi. Scraper yazmanın gerçek maliyeti kod
// yazma süresi değil, altı ay sonra "0 dosya indi" mesajını görüp nedenini
// bilmemek. doctor o tahmini ortadan kaldırıyor: hangi katmanın koptuğunu
// saniyeler içinde söylüyor ve kırılan yanıtı diske kaydediyor.
package doctor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// Named, bir resolver'ı adıyla eşler. site.Resolver adını taşımıyor.
type Named struct {
	Name     string
	Resolver site.Resolver
}

// Report, tek bir sitenin teşhis sonucu.
type Report struct {
	Site    string
	Results []site.LayerResult
	// Err, Diagnose'un kendisi çalışamadığında dolu olur (canary listesi boş gibi).
	Err error
}

// Worst, rapordaki en kötü durumu döndürür.
func (r Report) Worst() site.LayerStatus {
	if r.Err != nil {
		return site.StatusFail
	}
	worst := site.StatusOK
	for _, res := range r.Results {
		switch res.Status {
		case site.StatusFail:
			return site.StatusFail
		case site.StatusWarn:
			worst = site.StatusWarn
		}
	}
	return worst
}

// Run, her site için teşhis çalıştırır.
//
// Siteler SIRAYLA çalışıyor, paralel değil: doctor'ın çıktısı insan için ve
// karışık sıralı satırlar teşhisi zorlaştırır. Ayrıca teşhis sırasında rate
// limit'e çarpmak, teşhis edilen şeyi bozmak olurdu.
func Run(ctx context.Context, sites []Named) []Report {
	out := make([]Report, 0, len(sites))
	for _, s := range sites {
		res, err := s.Resolver.Diagnose(ctx)
		out = append(out, Report{Site: s.Name, Results: res, Err: err})
	}
	return out
}

// Format, raporları yazar ve en kötü durumu döndürür.
func Format(w io.Writer, reports []Report) site.LayerStatus {
	worst := site.StatusOK
	for i, r := range reports {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if s := r.Worst(); s == site.StatusFail {
			worst = site.StatusFail
		} else if s == site.StatusWarn && worst != site.StatusFail {
			worst = site.StatusWarn
		}

		fmt.Fprintf(w, "%s\n", r.Site)
		if r.Err != nil {
			fmt.Fprintf(w, "  %-10s %-5s %s\n", "-", site.StatusFail, r.Err)
			continue
		}
		if len(r.Results) == 0 {
			fmt.Fprintf(w, "  %-10s %-5s %s\n", "-", site.StatusWarn, "teşhis sonucu yok")
			continue
		}
		for _, res := range r.Results {
			fmt.Fprintf(w, "  %-10s %-5s %s\n", res.Layer, res.Status, res.Detail)
			if res.Evidence != "" {
				fmt.Fprintf(w, "  %-10s %-5s   %s\n", "", "", res.Evidence)
			}
		}
	}
	return worst
}

// Recorder, --record ile etkinleşen yanıt kaydedicisi.
//
// Kaydedilen dosyalar testdata'ya OTOMATİK kopyalanmıyor. Bu bilinçli: kayıt
// gerçek bir yanıt ve içinde dosya adları gibi içerik izleri olabilir. Neyin
// fixture olacağına insan karar verir; dağıtılan binary de kaynak ağacının
// içinde değildir.
type Recorder struct {
	Dir string

	mu    sync.Mutex
	saved []string
	errs  []error
}

// DefaultDir, --record-dir verilmediğinde kullanılır.
const DefaultDir = "recordings"

// For, belirli bir site için kayıt fonksiyonu üretir.
func (r *Recorder) For(siteName string) func(name string, data []byte) {
	return func(name string, data []byte) {
		r.save(siteName, name, data)
	}
}

func (r *Recorder) save(siteName, name string, data []byte) {
	dir := r.Dir
	if dir == "" {
		dir = DefaultDir
	}
	// Zaman damgası ad çakışmasını önlüyor ve diff alırken hangi kaydın daha
	// yeni olduğunu söylüyor.
	stamp := time.Now().UTC().Format("20060102-150405")
	fname := fmt.Sprintf("%s-%s-%s", stamp, safe(siteName), safe(name))
	path := filepath.Join(dir, fname)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", dir, err))
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", path, err))
		return
	}
	r.saved = append(r.saved, path)
}

// Saved, kaydedilen dosya yollarını döndürür.
func (r *Recorder) Saved() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.saved))
	copy(out, r.saved)
	sort.Strings(out)
	return out
}

// Errs, kayıt sırasında oluşan hataları döndürür.
// Kayıt hatası teşhisi DÜŞÜRMEZ: asıl iş katman raporu, kayıt yardımcı.
func (r *Recorder) Errs() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]error, len(r.errs))
	copy(out, r.errs)
	return out
}

// safe, dosya adı için asgari temizlik. Tam Windows temizliği dl.Component'te
// ama doctor oraya bağımlı olmasın: kayıt adları bizim ürettiğimiz kısa
// etiketler, site adı ve dosya adı.
func safe(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "kayit"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
