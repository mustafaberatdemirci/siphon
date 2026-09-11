package run

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// Worker, tek bir site için indirme bileşenlerini bir arada tutar: indirici,
// host başına eşzamanlılık sınırı ve yeniden deneme politikası.
//
// Neden ayrı bir tip: iki çağıran var. Komut satırı toplu koşu yapıyor (bir
// URL çöz, item'larını indir, bitir); pencere ise kalıcı bir kuyruk sürüyor
// (item'lar tek tek başlar, duraklar, devam eder). İkisi de tek bir item'ı
// AYNI şekilde indirmek zorunda: kayıt kontrolü, host sınırı, politika,
// kayıt yazma. Bu mantık iki yerde yaşasaydı zamanla ayrışırdı.
type Worker struct {
	Resolver site.Resolver
	Cfg      site.SiteConfig
	Down     *dl.Downloader
	Limiter  *snet.HostLimiter
	Policy   snet.Policy
}

// NewWorker, resolver'ın opsiyonel arayüzlerini indiriciye bağlayarak kurar.
func NewWorker(r site.Resolver, cfg site.SiteConfig, client *http.Client, ev Events) *Worker {
	down := &dl.Downloader{
		Client:    client,
		Logf:      ev.Debugf,
		Reresolve: r.ResolveOne,
		UserAgent: cfg.UserAgent,
		Progress:  ev.Progress,
	}
	// Resolver 403'ü siteye özgü yorumlayabiliyorsa indiriciye bağla; yoksa
	// her 403 "imzalı URL süresi doldu" sayılır ve rate limit derinleşir.
	if c, ok := r.(site.StatusClassifier); ok {
		down.Classify = c.ClassifyStatus
	}
	// Adresin isteğin tam öncesinde hazırlanması gerekiyorsa (bunkr'da süreli
	// imza) indiriciye bağlanıyor.
	if p, ok := r.(site.URLPreparer); ok {
		down.PrepareURL = p.PrepareURL
	}
	// Gövde diske yazılmadan önce çözülmesi gerekiyorsa (mega: AES-CTR).
	if dec, ok := r.(site.StreamDecoder); ok {
		down.Decode = dec.DecodeStream
	}
	// bunkr 200 ile bakım placeholder'ı döndürebiliyor; durum kodu yeterli
	// sinyal değil.
	if v, ok := r.(site.ResponseValidator); ok {
		down.Validate = v.ValidateResponse
	}

	// Sınır HOST başına: bir albüm birden fazla CDN host'una yayılabiliyor
	// ve tek bir genel sayaç yanlış yerde daraltma yapar.
	limiter := snet.NewHostLimiter(cfg.MaxConcurrent)

	// Parçalı indirme: site tavanı kadar bağlantı; ek parçalar host
	// yuvalarına tabi (beklemeden alınır, yoksa daha az parça).
	down.Segments = cfg.MaxSegments
	down.AcquireExtra = func(rawURL string, want int) (int, func()) {
		return limiter.TryAcquire(snet.HostOf(rawURL), want)
	}

	return &Worker{
		Resolver: r,
		Cfg:      cfg,
		Down:     down,
		Limiter:  limiter,
		Policy: snet.Policy{
			MaxAttempts: cfg.MaxRetries,
			MaxElapsed:  cfg.MaxElapsed,
			Backoff:     snet.Backoff{Base: cfg.BaseDelay, Max: cfg.MaxDelay},
			Logf:        ev.Debugf,
		},
	}
}

// OutcomeKind, tek item'ın nasıl bittiği.
type OutcomeKind int

const (
	OutcomeDone     OutcomeKind = iota // indi ve kaydedildi
	OutcomeSkipped                     // kayıt zaten vardı, dosya yerinde
	OutcomeFailed                      // kalıcı hata
	OutcomeStopped                     // captcha: koşu durmalı
	OutcomeCanceled                    // context iptali (kullanıcı durdurdu)
)

// Outcome, DownloadItem'ın sonucu.
type Outcome struct {
	Kind   OutcomeKind
	Result dl.Result   // Done'da
	Entry  store.Entry // Skipped'ta
	Err    error       // Failed/Stopped/Canceled'da
	// Degraded: dosya indi ama kaydı yazılamadı. İndirme geçerli, idempotence
	// bozuk; sonraki koşu bunu yeniden indirir.
	Degraded bool
}

// DownloadItem, tek bir item'ı baştan sona işler: kayıt kontrolü, host
// sınırı, politika ile indirme, kayıt yazma. Olayları ev üzerinden bildirir.
//
// ledger nil olabilir (kayıt tutulmayan çağıran); o zaman atlama kontrolü ve
// kayıt yazma yapılmaz.
func (w *Worker) DownloadItem(ctx context.Context, outDir string, ledger *store.Ledger, it site.Item, ev Events) Outcome {
	// Zaten indirilmiş mi? Kayda tek başına GÜVENİLMİYOR: dosyanın gerçekten
	// yerinde olduğu da kontrol ediliyor. Kullanıcı dosyayı silmişse kayda
	// bakıp atlamak sessiz başarısızlık olur.
	if ledger != nil {
		if e, ok := ledger.Lookup(it.Dir, it.SourcePage, it.Filename); ok {
			if fi, serr := os.Stat(filepath.Join(outDir, e.Path)); serr == nil && !fi.IsDir() {
				if ev.ItemSkipped != nil {
					ev.ItemSkipped(it, e)
				}
				ev.debugf("[%d] %s zaten kayıtlı, atlanıyor", it.Index+1, e.Filename)
				return Outcome{Kind: OutcomeSkipped, Entry: e}
			}
			ev.errorf("  kayıt %q diyor ama dosya yok, yeniden indiriliyor", e.Path)
		}
	}

	release, aerr := w.Limiter.Acquire(ctx, snet.HostOf(it.URL))
	if aerr != nil {
		return Outcome{Kind: OutcomeCanceled, Err: aerr}
	}
	defer release()

	if ev.ItemStarted != nil {
		ev.ItemStarted(it)
	}

	var res dl.Result
	derr := w.Policy.Do(ctx, func(int) error {
		var e error
		res, e = w.Down.Download(ctx, outDir, it)
		return e
	})

	switch {
	case derr == nil:
		out := Outcome{Kind: OutcomeDone, Result: res}
		if ledger != nil {
			// Yol GÖRELİ kaydediliyor: çıktı klasörü taşındığında kayıt
			// geçerli kalsın.
			rel, relErr := filepath.Rel(outDir, res.Path)
			if relErr != nil {
				rel = filepath.Base(res.Path)
			}
			if lerr := ledger.Add(store.Entry{
				SourcePage: it.SourcePage,
				Dir:        it.Dir,
				Filename:   it.Filename,
				Path:       rel,
				Size:       res.Size,
				SHA256:     res.SHA256,
			}); lerr != nil {
				ev.errorf("  %s indi ama kaydı yazılamadı: %v", rel, lerr)
				out.Degraded = true
			}
		}
		if ev.ItemDone != nil {
			ev.ItemDone(it, res)
		}
		ev.infof("[%d] %s OK", it.Index+1, filepath.Base(res.Path))
		return out

	case errors.Is(derr, snet.ErrStop):
		// Captcha. Beklemek çözmez ve denemeye devam etmek durumu
		// kötüleştirir; koşuyu durdur.
		ev.errorf("DURDURULDU: %v", derr)
		return Outcome{Kind: OutcomeStopped, Err: derr}

	case errors.Is(derr, context.Canceled):
		return Outcome{Kind: OutcomeCanceled, Err: derr}

	default:
		// Albüm içinde ölü item albümü düşürmez.
		ev.errorf("  %s: %v", it.Filename, derr)
		if ev.ItemFailed != nil {
			ev.ItemFailed(it, derr)
		}
		return Outcome{Kind: OutcomeFailed, Err: derr}
	}
}
