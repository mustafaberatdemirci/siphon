// Package run, indirme hattını tutar: config yükleme, resolver dispatch,
// eşzamanlılık, yeniden deneme, kayıt ve çıkış kodu muhasebesi.
//
// Neden ayrı bir paket: iki arayüz var (komut satırı ve pencere) ve ikisi de
// AYNI hattı çalıştırmak zorunda. Hat main.go içinde kalsaydı pencere sürümü
// onu kopyalamak zorunda kalırdı; iki kopya zamanla ayrışır ve aynı linkte
// farklı davranan iki program ortaya çıkar. Bu paketin tek varlık sebebi o
// ayrışmayı imkânsız kılmak.
//
// Arayüze özgü hiçbir şey içermez: burada fmt.Println yok, pencere yok.
// Olan biten Events üzerinden dışarı bildiriliyor.
package run

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/mustafaberatdemirci/siphon/internal/config"
	"github.com/mustafaberatdemirci/siphon/internal/dl"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/site"
	"github.com/mustafaberatdemirci/siphon/internal/store"
)

// DefaultMaxInFlight, aynı anda canlı tutulacak indirme goroutine'i sayısı.
// Gerçek daraltmayı HostLimiter yapıyor; bu yalnızca 5000 item'lık bir albümde
// 5000 goroutine açmamak için var.
const DefaultMaxInFlight = 64

// Options, bir koşunun girdisi.
type Options struct {
	URLs        []string
	OutDir      string
	ConfigPath  string
	ResolveOnly bool
	MaxInFlight int
}

// Events, koşu sırasında olan biteni dışarı bildirir.
// Tüm alanlar nil olabilir; nil olan sessizce atlanır.
type Events struct {
	// Debugf ayrıntı, Infof normal ilerleme, Errorf hata.
	Debugf func(format string, a ...any)
	Infof  func(format string, a ...any)
	Errorf func(format string, a ...any)

	// ConfigLoaded, hangi config'in yüklendiğini bildirir.
	ConfigLoaded func(source string, sites int)
	// LedgerOpened, kaydın yolunu ve içindeki item sayısını bildirir.
	LedgerOpened func(path string, items, skippedLines int)

	// ItemResolved, --resolve-only modunda her item için.
	ItemResolved func(it site.Item)

	// ItemQueued, item çözülür çözülmez, indirme kuyruğuna girerken çağrılır.
	//
	// URLResolved'dan AYRI olmak zorunda: URLResolved bir URL'in TÜM item'ları
	// bitince tetikleniyor, yani arayüz toplam sayıyı ancak iş bittikten sonra
	// öğrenirdi ve ilerleme çubuğu koşu boyunca sıfırda kalırdı. Bu tam olarak
	// kullanıcının bildirdiği hataydı.
	ItemQueued func(it site.Item)

	// ItemStarted, indirme kuyruğundan çıkıp gerçekten başladığında.
	ItemStarted func(it site.Item)
	// Progress, transfer sürerken periyodik. total bilinmiyorsa -1.
	Progress func(it site.Item, done, total int64)
	// ItemDone, dosya doğrulanıp nihai adına taşındığında.
	ItemDone func(it site.Item, res dl.Result)
	// ItemFailed, item kalıcı olarak başarısız olduğunda.
	ItemFailed func(it site.Item, err error)
	// ItemSkipped, kayıt zaten indirildiğini söylediğinde.
	ItemSkipped func(it site.Item, e store.Entry)
}

func (e Events) debugf(f string, a ...any) {
	if e.Debugf != nil {
		e.Debugf(f, a...)
	}
}

func (e Events) infof(f string, a ...any) {
	if e.Infof != nil {
		e.Infof(f, a...)
	}
}

func (e Events) errorf(f string, a ...any) {
	if e.Errorf != nil {
		e.Errorf(f, a...)
	}
}

// Summary, koşunun sonucu.
type Summary struct {
	URLs     int // denenen URL sayısı
	Items    int // çözülen item sayısı
	Done     int // inen dosya
	Failed   int // kalıcı başarısız item
	Skipped  int // kayıt gereği atlanan
	Degraded int // indi ama kaydı yazılamadı
	// SkippedURLs, hiçbir resolver'a uymadığı için atlanan URL sayısı.
	SkippedURLs int
	// ResolvedAny, en az bir URL item ürettiyse true.
	ResolvedAny bool
	// Halted, koşu sinyalle veya captcha ile kesildiyse true.
	Halted bool
	// ResolveErrors, URL seviyesinde çözümleme hatası sayısı.
	ResolveErrors int
	// ItemErrors, resolver'ın bildirdiği item seviyesi hata sayısı.
	ItemErrors int
}

// Problems, çıkış kodunu etkileyen sorun sayısı.
func (s Summary) Problems() int {
	return s.Failed + s.Degraded + s.SkippedURLs + s.ResolveErrors + s.ItemErrors
}

// Çıkış kodu sözleşmesi. 2 ile 3 kasıtlı olarak ayrı: 2 "site tarafında bir şey
// oldu", 3 "girdi yanlış" demek ve ikisi tamamen farklı teşhis yolları.
const (
	ExitOK      = 0 // her URL çözüldü ve her item indi
	ExitPartial = 1 // kısmi başarısızlık, atlanan URL, veya sinyalle kesilme
	ExitNoneOK  = 2 // hiçbir URL çözümlenemedi
	ExitUsage   = 3 // geçersiz TOML, schema_version uyuşmazlığı, okunamayan girdi
)

// ExitCode, özeti çıkış koduna çevirir.
func (s Summary) ExitCode() int {
	switch {
	case s.Halted:
		return ExitPartial
	case !s.ResolvedAny:
		return ExitNoneOK
	case s.Problems() > 0:
		return ExitPartial
	default:
		return ExitOK
	}
}

// ErrUsage, çıkış kodu 3 ile eşleşen hata sınıfı.
var ErrUsage = config.ErrUsage

// Setup, config'i yükler ve resolver'ları kurar.
//
// canaries boş değilse config'teki canary listesi EZİLİR (doctor -canary).
// record nil olabilir; doctor --record ile doldurur.
func Setup(
	ev Events, cfgPath string,
	record func(siteName string) func(string, []byte),
	canaries []string,
) ([]site.SiteConfig, []site.Resolver, error) {
	cfgs, src, err := config.Load(config.Embedded, cfgPath)
	if err != nil {
		return nil, nil, err
	}
	if ev.ConfigLoaded != nil {
		ev.ConfigLoaded(src.String(), len(cfgs))
	}

	reg := site.NewRegistry()
	for name, factory := range map[string]site.Factory{
		site.PixeldrainName: site.NewPixeldrain,
		site.BunkrName:      site.NewBunkr,
	} {
		if rerr := reg.Register(name, factory); rerr != nil {
			return nil, nil, fmt.Errorf("registry: %w", rerr)
		}
	}

	for i := range cfgs {
		cfgs[i].Logf = ev.Debugf
		if record != nil {
			cfgs[i].Record = record(cfgs[i].Name)
		}
		if len(canaries) > 0 {
			cfgs[i].CanaryURLs = canaries
		}
	}
	resolvers, err := reg.Build(cfgs)
	if err != nil {
		return nil, nil, err
	}
	return cfgs, resolvers, nil
}

// Run, verilen URL'leri çözer ve indirir.
//
// Dönen hata YALNIZCA kullanım/konfigürasyon hatasıdır (çıkış 3). Site
// tarafındaki her şey Summary'ye yazılır; bu ayrım dokümanın çıkış kodu
// sözleşmesinin temeli.
func Run(ctx context.Context, opt Options, ev Events) (Summary, error) {
	var sum Summary

	if len(opt.URLs) == 0 {
		return sum, fmt.Errorf("%w: girdi yok", ErrUsage)
	}
	if opt.OutDir == "" {
		opt.OutDir = "."
	}
	inFlight := opt.MaxInFlight
	if inFlight <= 0 {
		inFlight = DefaultMaxInFlight
	}

	cfgs, resolvers, err := Setup(ev, opt.ConfigPath, nil, nil)
	if err != nil {
		return sum, err
	}

	httpClient := snet.NewClient()

	// Kayıt ÇIKTI KÖKÜNÜN altında açılıyor, cwd'de veya exe yanında değil:
	// farklı bir çıktı klasörüyle yapılan ikinci koşu aksi halde dosyalar orada
	// olmadığı halde "hepsi indi" deyip 0 dönerdi.
	//
	// resolve-only modunda HİÇ açılmıyor: o mod diske hiçbir şey yazmıyor ve
	// kaydı açmak çıktı klasörünü oluşturmak demek olurdu.
	var ledger *store.Ledger
	if !opt.ResolveOnly {
		l, lerr := store.Open(opt.OutDir)
		if lerr != nil {
			return sum, lerr
		}
		defer l.Close()
		ledger = l
		if ev.LedgerOpened != nil {
			ev.LedgerOpened(l.Path(), l.Len(), l.Skipped())
		}
		if n := l.Skipped(); n > 0 {
			ev.errorf("kayıtta okunamayan %d satır atlandı: %s", n, l.Path())
		}
	}

	for _, u := range opt.URLs {
		sum.URLs++

		r, idx := pick(resolvers, u)
		if r == nil {
			// Sessiz başarısızlık yok: atlanan URL çıkış kodunu etkiler.
			ev.errorf("eşleşen resolver yok, atlanıyor: %s", u)
			sum.SkippedURLs++
			continue
		}
		cfg := cfgs[idx]

		res := runOne(ctx, runCtx{
			url:      u,
			resolver: r,
			cfg:      cfg,
			client:   httpClient,
			ledger:   ledger,
			opt:      opt,
			ev:       ev,
			inFlight: inFlight,
		})

		sum.Items += res.count
		sum.Done += res.done
		sum.Failed += res.failed
		sum.Skipped += res.skipped
		sum.Degraded += res.degraded
		if res.count > 0 {
			sum.ResolvedAny = true
		}

		if res.halted {
			sum.Halted = true
			break
		}
		if res.resolveErr != nil {
			ev.errorf("%s: %v", u, res.resolveErr)
			if layer, ok := site.LayerOf(res.resolveErr); ok {
				ev.debugf("  kopan katman: %s", layer)
			}
			sum.ResolveErrors++
			continue
		}
		sum.ItemErrors += res.itemErrs
		if res.skipped > 0 {
			ev.infof("%s: %d item (%d zaten indirilmiş, atlandı)", u, res.count, res.skipped)
		} else {
			ev.infof("%s: %d item", u, res.count)
		}
	}
	return sum, nil
}

type runCtx struct {
	url      string
	resolver site.Resolver
	cfg      site.SiteConfig
	client   *http.Client
	ledger   *store.Ledger
	opt      Options
	ev       Events
	inFlight int
}

type oneResult struct {
	count      int
	done       int
	failed     int
	skipped    int
	degraded   int
	itemErrs   int
	halted     bool
	resolveErr error
}

// runOne, tek bir URL'i çözer ve item'larını indirir.
func runOne(ctx context.Context, rc runCtx) oneResult {
	var out oneResult
	r, cfg, ev, opt := rc.resolver, rc.cfg, rc.ev, rc.opt

	// Downloader URL başına kurulur: ad çakışma haritası albüm kapsamlı olmalı.
	down := &dl.Downloader{
		Client:    rc.client,
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
	// bunkr 200 ile bakım placeholder'ı döndürebiliyor; durum kodu yeterli
	// sinyal değil.
	// Adresin isteğin tam öncesinde hazırlanması gerekiyorsa (bunkr'da süreli
	// imza) indiriciye bağlanıyor.
	if p, ok := r.(site.URLPreparer); ok {
		down.PrepareURL = p.PrepareURL
	}
	if v, ok := r.(site.ResponseValidator); ok {
		down.Validate = v.ValidateResponse
	}

	// Sınır HOST başına: bir albüm birden fazla CDN host'una yayılabiliyor
	// ve tek bir genel sayaç yanlış yerde daraltma yapar.
	limiter := snet.NewHostLimiter(cfg.MaxConcurrent)
	policy := snet.Policy{
		MaxAttempts: cfg.MaxRetries,
		MaxElapsed:  cfg.MaxElapsed,
		Backoff:     snet.Backoff{Base: cfg.BaseDelay, Max: cfg.MaxDelay},
		Logf:        ev.Debugf,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(rc.inFlight)

	var (
		count   int // Resolve tek goroutine'den çağırıyor; atomic gerekmiyor
		done    atomic.Int64
		failed  atomic.Int64
		already atomic.Int64
		degrade atomic.Int64 // item indi ama kaydı yazılamadı
		stopped atomic.Bool
	)

	itemErrs, rerr := r.Resolve(ctx, rc.url, func(it site.Item) error {
		count++
		if opt.ResolveOnly {
			if ev.ItemResolved != nil {
				ev.ItemResolved(it)
			}
			return nil
		}
		// Kuyruğa girer girmez bildiriliyor: arayüz toplamı akış halinde
		// öğrensin, işin sonunu beklemesin.
		if ev.ItemQueued != nil {
			ev.ItemQueued(it)
		}
		g.Go(func() error {
			// Zaten indirilmiş mi? Kayda tek başına GÜVENİLMİYOR: dosyanın
			// gerçekten yerinde olduğu da kontrol ediliyor. Kullanıcı dosyayı
			// silmişse kayda bakıp atlamak sessiz başarısızlık olur.
			if e, ok := rc.ledger.Lookup(it.Dir, it.SourcePage, it.Filename); ok {
				if fi, serr := os.Stat(filepath.Join(opt.OutDir, e.Path)); serr == nil && !fi.IsDir() {
					already.Add(1)
					if ev.ItemSkipped != nil {
						ev.ItemSkipped(it, e)
					}
					ev.debugf("[%d] %s zaten kayıtlı, atlanıyor", it.Index+1, e.Filename)
					return nil
				}
				ev.errorf("  kayıt %q diyor ama dosya yok, yeniden indiriliyor", e.Path)
			}

			release, aerr := limiter.Acquire(gctx, snet.HostOf(it.URL))
			if aerr != nil {
				return aerr
			}
			defer release()

			if ev.ItemStarted != nil {
				ev.ItemStarted(it)
			}

			var res dl.Result
			derr := policy.Do(gctx, func(int) error {
				var e error
				res, e = down.Download(gctx, opt.OutDir, it)
				return e
			})

			switch {
			case derr == nil:
				// Yol GÖRELİ kaydediliyor: çıktı klasörü taşındığında kayıt
				// geçerli kalsın.
				rel, relErr := filepath.Rel(opt.OutDir, res.Path)
				if relErr != nil {
					rel = filepath.Base(res.Path)
				}
				if lerr := rc.ledger.Add(store.Entry{
					SourcePage: it.SourcePage,
					Dir:        it.Dir,
					Filename:   it.Filename,
					Path:       rel,
					Size:       res.Size,
					SHA256:     res.SHA256,
				}); lerr != nil {
					// Dosya indi ama kaydı yazılamadı: indirme geçerli,
					// idempotence bozuk. Sonraki koşu bunu yeniden indirir.
					ev.errorf("  %s indi ama kaydı yazılamadı: %v", rel, lerr)
					degrade.Add(1)
				}
				done.Add(1)
				if ev.ItemDone != nil {
					ev.ItemDone(it, res)
				}
				ev.infof("[%d] %s OK", it.Index+1, filepath.Base(res.Path))
				return nil

			case errors.Is(derr, snet.ErrStop):
				// Captcha. Beklemek çözmez ve denemeye devam etmek durumu
				// kötüleştirir; koşuyu durdur.
				ev.errorf("DURDURULDU: %v", derr)
				stopped.Store(true)
				return derr // gctx iptal edilir, kalan işler durur

			case errors.Is(derr, context.Canceled):
				return derr

			default:
				// Albüm içinde ölü item albümü düşürmez.
				ev.errorf("  %s: %v", it.Filename, derr)
				failed.Add(1)
				if ev.ItemFailed != nil {
					ev.ItemFailed(it, derr)
				}
				return nil
			}
		})
		return nil
	})

	// Resolve döndü ama indirmeler sürüyor olabilir; her durumda bekle.
	gerr := g.Wait()

	out.count = count
	out.done = int(done.Load())
	out.failed = int(failed.Load())
	out.skipped = int(already.Load())
	out.degraded = int(degrade.Load())
	out.itemErrs = len(itemErrs)

	for _, ie := range itemErrs {
		ev.errorf("item: %v", ie)
	}

	switch {
	case stopped.Load():
		out.halted = true
	case gerr != nil && errors.Is(gerr, context.Canceled):
		ev.errorf("kesildi: %s", rc.url)
		out.halted = true
	case rerr != nil:
		if errors.Is(rerr, context.Canceled) {
			ev.errorf("kesildi: %s", rc.url)
			out.halted = true
		} else {
			out.resolveErr = rerr
		}
	}
	return out
}

// pick, URL'e uyan ilk resolver'ı ve onun config indeksini döndürür.
// Registry.Build sırayı koruduğu için indeks cfgs ile birebir eşleşir.
func pick(rs []site.Resolver, u string) (site.Resolver, int) {
	for i, r := range rs {
		if r.Match(u) {
			return r, i
		}
	}
	return nil, -1
}
