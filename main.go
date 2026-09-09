// Siphon: pixeldrain ve bunkr linklerini toplu indirir.
//
// Bu dosya orkestrasyondur: bayraklar, girdi okuma, registry dispatch ve çıkış
// kodu sözleşmesi. İndirme mantığı internal/dl'de.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	_ "embed"

	"github.com/mustafaberatdemirci/siphon/internal/config"
	"github.com/mustafaberatdemirci/siphon/internal/dl"
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

	httpClient := newHTTPClient()

	var (
		resolvedAny bool
		problems    int
		interrupted bool
	)

	for _, u := range urls {
		r, idx := pick(resolvers, u)
		if r == nil {
			// Sessiz başarısızlık yok: atlanan URL çıkış kodunu etkiler.
			log.errorf("eşleşen resolver yok, atlanıyor: %s", u)
			problems++
			continue
		}

		// Downloader URL başına kurulur: claimed haritası albüm kapsamlı olmalı.
		//
		// Client açıkça veriliyor: http.DefaultClient'a bırakmak, sites.toml'daki
		// user_agent'ın yalnızca API çağrılarına uygulanması ve transferlerde
		// hiçbir zaman aşımı olmaması demekti.
		down := &dl.Downloader{
			Client:    httpClient,
			Logf:      log.debugf,
			Reresolve: r.ResolveOne,
			UserAgent: cfgs[idx].UserAgent,
		}
		// Resolver 403'ü siteye özgü yorumlayabiliyorsa indiriciye bağla; yoksa
		// her 403 "imzalı URL süresi doldu" sayılır ve rate limit derinleşir.
		if c, ok := r.(site.StatusClassifier); ok {
			down.Classify = c.ClassifyStatus
		}

		var count, failed int
		itemErrs, err := r.Resolve(ctx, u, func(it site.Item) error {
			count++
			if resolveOnly {
				fmt.Fprintln(os.Stdout, it.URL)
				return nil
			}
			// Windows ad temizliği dl.Download içinde yapılıyor: kural diske
			// yazan kodla aynı yerde durmalı, burada değil.
			if derr := down.Download(ctx, outDir, it); derr != nil {
				if errors.Is(derr, context.Canceled) {
					return derr
				}
				// Albüm içinde ölü item albümü düşürmez, sadece çıkış kodunu etkiler.
				log.errorf("  %s: %v", it.Filename, derr)
				failed++
				return nil
			}
			log.infof("[%d] %s OK", it.Index+1, it.Filename)
			return nil
		})
		problems += failed

		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.errorf("kesildi: %s", u)
				interrupted = true
				break
			}
			// LayerError.Error() katman adını zaten içeriyor; tekrar önekleme.
			log.errorf("%s: %v", u, err)
			if layer, ok := site.LayerOf(err); ok {
				log.debugf("  kopan katman: %s", layer)
			}
			problems++
			continue
		}
		for _, ie := range itemErrs {
			log.errorf("item: %v", ie)
			problems++
		}
		if count > 0 {
			resolvedAny = true
		}
		log.infof("%s: %d item", u, count)
	}

	switch {
	case interrupted:
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

// newHTTPClient, transfer istekleri için istemciyi kurar.
//
// Client.Timeout KASITLI olarak verilmiyor: o alan gövde okumayı da kapsar ve
// birkaç gigabaytlık bir indirmeyi ortasından keser. Zaman aşımları bağlantı
// kurma ve yanıt başlığı seviyesinde tutuluyor; takılan bir sunucu yakalanır,
// yavaş ama çalışan bir transfer kesilmez.
//
// SiteConfig.MaxElapsed burada uygulanmıyor: o, item başına YENİDEN DENEME
// bütçesi (adım 6), transfer için son tarih değil.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
		},
	}
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
