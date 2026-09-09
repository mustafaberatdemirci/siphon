// Siphon: pixeldrain ve bunkr linklerini toplu indirir.
//
// Bu dosya orkestrasyondur: bayraklar, girdi okuma, registry dispatch ve çıkış
// kodu sözleşmesi. İndirme mantığı internal/dl'de, eşzamanlılık sınırı ve
// backoff internal/net'te.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"

	_ "embed"

	"golang.org/x/sync/errgroup"

	"github.com/mustafaberatdemirci/siphon/internal/config"
	"github.com/mustafaberatdemirci/siphon/internal/dl"
	snet "github.com/mustafaberatdemirci/siphon/internal/net"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

//go:embed sites.toml
var embeddedSites []byte

// Çıkış kodu sözleşmesi. 2 ile 3 kasıtlı olarak ayrı: 2 "site tarafında bir şey
// oldu", 3 "girdi yanlış" demek ve ikisi tamamen farklı teşhis yolları.
const (
	exitOK      = 0 // her URL çözüldü ve her item indi
	exitPartial = 1 // kısmi başarısızlık, atlanan URL, veya sinyalle kesilme
	exitNoneOK  = 2 // hiçbir URL çözümlenemedi
	exitUsage   = 3 // geçersiz TOML, schema_version uyuşmazlığı, okunamayan -i
)

// maxInFlight, aynı anda canlı tutulacak indirme goroutine'i sayısının üst
// sınırıdır. Gerçek daraltmayı HostLimiter yapıyor; bu yalnızca 5000 item'lık
// bir albümde 5000 goroutine açmamak için var.
const maxInFlight = 64

type logger struct {
	verbose bool
	quiet   bool
}

func (l logger) infof(format string, a ...any) {
	if l.quiet {
		return
	}
	fmt.Fprintf(os.Stdout, format+"\n", a...)
}

func (l logger) debugf(format string, a ...any) {
	if !l.verbose || l.quiet {
		return
	}
	fmt.Fprintf(os.Stdout, format+"\n", a...)
}

func (l logger) errorf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

func main() { os.Exit(run()) }

func run() int {
	var (
		inputPath   string
		outDir      string
		cfgPath     string
		verbose     bool
		quiet       bool
		resolveOnly bool
	)
	flag.StringVar(&inputPath, "i", "", "URL listesi dosyası (satır başına bir URL, # ile yorum)")
	flag.StringVar(&outDir, "out", ".", "çıktı kökü")
	flag.StringVar(&cfgPath, "c", "", "sites.toml yolu (verilmezse exe yanı, sonra cwd, sonra gömülü)")
	flag.BoolVar(&verbose, "v", false, "katman detayını da bas")
	flag.BoolVar(&quiet, "q", false, "sadece hataları bas")
	flag.BoolVar(&resolveOnly, "resolve-only", false, "çözümlenen URL'leri bas, indirme")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "kullanım: siphon [bayraklar] [url ...]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	log := logger{verbose: verbose, quiet: quiet}

	// Ctrl+C: context iptal edilir, indirici .part'ı sync edip state yazar.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfgs, src, err := config.Load(embeddedSites, cfgPath)
	if err != nil {
		log.errorf("%v", err)
		return exitUsage
	}
	log.debugf("config: %s (%d site)", src, len(cfgs))

	reg := site.NewRegistry()
	if err := reg.Register(site.PixeldrainName, site.NewPixeldrain); err != nil {
		log.errorf("registry: %v", err)
		return exitUsage
	}
	resolvers, err := reg.Build(cfgs)
	if err != nil {
		log.errorf("%v", err)
		return exitUsage
	}

	urls, err := readURLs(inputPath, flag.Args())
	if err != nil {
		log.errorf("%v", err)
		return exitUsage
	}
	if len(urls) == 0 {
		log.errorf("girdi yok: -i ile dosya ver veya URL'leri argüman olarak geç")
		flag.Usage()
		return exitUsage
	}

	httpClient := snet.NewClient()

	var (
		resolvedAny bool
		problems    int
		halted      bool
	)

	for _, u := range urls {
		r, idx := pick(resolvers, u)
		if r == nil {
			// Sessiz başarısızlık yok: atlanan URL çıkış kodunu etkiler.
			log.errorf("eşleşen resolver yok, atlanıyor: %s", u)
			problems++
			continue
		}
		cfg := cfgs[idx]

		// Downloader URL başına kurulur: claimed haritası albüm kapsamlı olmalı.
		down := &dl.Downloader{
			Client:    httpClient,
			Logf:      log.debugf,
			Reresolve: r.ResolveOne,
			UserAgent: cfg.UserAgent,
		}
		// Resolver 403'ü siteye özgü yorumlayabiliyorsa indiriciye bağla; yoksa
		// her 403 "imzalı URL süresi doldu" sayılır ve rate limit derinleşir.
		if c, ok := r.(site.StatusClassifier); ok {
			down.Classify = c.ClassifyStatus
		}

		// Sınır HOST başına: bir albüm birden fazla CDN host'una yayılabiliyor
		// ve tek bir genel sayaç yanlış yerde daraltma yapar.
		limiter := snet.NewHostLimiter(cfg.MaxConcurrent)
		policy := snet.Policy{
			MaxAttempts: cfg.MaxRetries,
			MaxElapsed:  cfg.MaxElapsed,
			Backoff:     snet.Backoff{Base: cfg.BaseDelay, Max: cfg.MaxDelay},
			Logf:        log.debugf,
		}

		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(maxInFlight)

		var (
			count   int // Resolve tek goroutine'den çağırıyor; atomic gerekmiyor
			failed  atomic.Int64
			stopped atomic.Bool
		)

		itemErrs, rerr := r.Resolve(ctx, u, func(it site.Item) error {
			count++
			if resolveOnly {
				fmt.Fprintln(os.Stdout, it.URL)
				return nil
			}
			g.Go(func() error {
				release, aerr := limiter.Acquire(gctx, snet.HostOf(it.URL))
				if aerr != nil {
					return aerr
				}
				defer release()

				var final string
				derr := policy.Do(gctx, func(int) error {
					var e error
					final, e = down.Download(gctx, outDir, it)
					return e
				})

				switch {
				case derr == nil:
					// Diskteki ad temizleyiciden geçtiği için girdi adından
					// farklı olabilir; gerçek adı basıyoruz.
					log.infof("[%d] %s OK", it.Index+1, filepath.Base(final))
					return nil
				case errors.Is(derr, snet.ErrStop):
					// Captcha. Beklemek çözmez ve denemeye devam etmek durumu
					// kötüleştirir; koşuyu durdur.
					log.errorf("DURDURULDU: %v", derr)
					stopped.Store(true)
					return derr // gctx iptal edilir, kalan işler durur
				case errors.Is(derr, context.Canceled):
					return derr
				default:
					// Albüm içinde ölü item albümü düşürmez.
					log.errorf("  %s: %v", it.Filename, derr)
					failed.Add(1)
					return nil
				}
			})
			return nil
		})

		// Resolve döndü ama indirmeler sürüyor olabilir; her durumda bekle.
		gerr := g.Wait()
		problems += int(failed.Load())
		if count > 0 {
			resolvedAny = true
		}

		if stopped.Load() {
			halted = true
			break
		}
		if gerr != nil && errors.Is(gerr, context.Canceled) {
			log.errorf("kesildi: %s", u)
			halted = true
			break
		}
		if rerr != nil {
			if errors.Is(rerr, context.Canceled) {
				log.errorf("kesildi: %s", u)
				halted = true
				break
			}
			// LayerError.Error() katman adını zaten içeriyor; tekrar önekleme.
			log.errorf("%s: %v", u, rerr)
			if layer, ok := site.LayerOf(rerr); ok {
				log.debugf("  kopan katman: %s", layer)
			}
			problems++
			continue
		}
		for _, ie := range itemErrs {
			log.errorf("item: %v", ie)
			problems++
		}
		log.infof("%s: %d item", u, count)
	}

	switch {
	case halted:
		return exitPartial
	case !resolvedAny:
		return exitNoneOK
	case problems > 0:
		return exitPartial
	default:
		return exitOK
	}
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

// readURLs, -i dosyasını ve konumsal argümanları birleştirir.
// Boş satırlar ve # ile başlayanlar atlanır.
func readURLs(path string, args []string) ([]string, error) {
	out := append([]string{}, args...)
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: -i dosyası okunamadı: %v", config.ErrUsage, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}
