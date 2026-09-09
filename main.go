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
	"github.com/mustafaberatdemirci/siphon/internal/doctor"
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

func main() {
	// Alt komut, flag.Parse'tan ÖNCE ayrılıyor. Go'nun flag paketi ilk
	// bayrak olmayan argümanda duruyor, yani "siphon doctor -v" biçimini
	// tek bir FlagSet ile ayrıştırmak mümkün değil.
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(runDoctor(os.Args[2:]))
	}
	os.Exit(run())
}

// setupResolvers, iki komutun da ihtiyaç duyduğu ortak kurulumu yapar:
// config yükleme, registry kaydı, resolver inşası.
//
// record nil olabilir; doctor --record ile doldurur.
// canaries boş değilse config'teki canary listesi EZİLİR (doctor -canary).
func setupResolvers(
	log logger, cfgPath string,
	record func(siteName string) func(string, []byte),
	canaries []string,
) ([]site.SiteConfig, []site.Resolver, error) {
	cfgs, src, err := config.Load(embeddedSites, cfgPath)
	if err != nil {
		return nil, nil, err
	}
	log.debugf("config: %s (%d site)", src, len(cfgs))

	reg := site.NewRegistry()
	for name, factory := range map[string]site.Factory{
		site.PixeldrainName: site.NewPixeldrain,
		site.BunkrName:      site.NewBunkr,
	} {
		if err := reg.Register(name, factory); err != nil {
			return nil, nil, fmt.Errorf("registry: %w", err)
		}
	}
	// Teşhis satırları -v ile görünür olsun: domain rotasyonu sessizce olursa
	// "neden başka bir domaine gitti" sorusu cevaplanamaz.
	for i := range cfgs {
		cfgs[i].Logf = log.debugf
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

// runDoctor, katman teşhisini çalıştırır.
//
// Çıkış kodu: 0 hiç FAIL yok, 1 en az bir FAIL, 3 konfigürasyon hatası.
// WARN çıkış kodunu ETKİLEMEZ ve bu kasıtlı: "CDN bir kapı değil sinyal"
// kuralı ancak WARN başarısızlık sayılmazsa anlam taşıyor. Yeni bir CDN host'u
// görmek betiği kırmamalı; kullanıcıya söylemeli.
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var (
		cfgPath   string
		verbose   bool
		quiet     bool
		record    bool
		recordDir string
	)
	fs.StringVar(&cfgPath, "c", "", "sites.toml yolu")
	fs.BoolVar(&verbose, "v", false, "teşhis detayını da bas")
	fs.BoolVar(&quiet, "q", false, "sadece hataları bas")
	fs.BoolVar(&record, "record", false, "yanıtları diske kaydet (diff için)")
	fs.StringVar(&recordDir, "record-dir", doctor.DefaultDir, "kayıt klasörü")
	// Neden bir bayrak gerekiyor: canary_urls bir dizi alanı ve dizi alanları
	// config birleştirmede BİRLEŞİYOR (union, sıra korunur). Dış dosyaya canary
	// yazmak onu listenin SONUNA ekliyor ve Diagnose ilk çalışan canary'de
	// durduğu için oraya hiç gelinmiyor. Birleştirme semantiği domainler için
	// doğru (eklemek istiyorsun, değiştirmek değil), ama "şu albüm kırık mı"
	// diye sormanın bir yolu olmak zorunda.
	var canaries stringList
	fs.Var(&canaries, "canary", "canary URL'ini değiştir (tekrarlanabilir)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "kullanım: siphon doctor [bayraklar] [site ...]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	log := logger{verbose: verbose, quiet: quiet}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var rec *doctor.Recorder
	var recFn func(string) func(string, []byte)
	if record {
		rec = &doctor.Recorder{Dir: recordDir}
		recFn = rec.For
	}

	cfgs, resolvers, err := setupResolvers(log, cfgPath, recFn, canaries)
	if err != nil {
		log.errorf("%v", err)
		return exitUsage
	}

	// Argüman verilmişse yalnızca o siteler teşhis edilir.
	want := map[string]bool{}
	for _, a := range fs.Args() {
		want[strings.ToLower(a)] = true
	}
	var sites []doctor.Named
	for i, r := range resolvers {
		name := cfgs[i].Name
		if len(want) > 0 && !want[strings.ToLower(name)] {
			continue
		}
		sites = append(sites, doctor.Named{Name: name, Resolver: r})
	}
	if len(sites) == 0 {
		log.errorf("teşhis edilecek site yok (bilinen: %s)", strings.Join(siteNames(cfgs), ", "))
		return exitUsage
	}

	reports := doctor.Run(ctx, sites)
	worst := doctor.Format(os.Stdout, reports)

	if rec != nil {
		for _, f := range rec.Saved() {
			log.infof("kaydedildi: %s", f)
		}
		// Kayıt hatası teşhisi DÜŞÜRMEZ: asıl iş katman raporu.
		for _, e := range rec.Errs() {
			log.errorf("kayıt hatası: %v", e)
		}
		if len(rec.Saved()) == 0 && len(rec.Errs()) == 0 {
			log.errorf("kayıt istendi ama hiçbir yanıt kaydedilmedi")
		}
	}

	if worst == site.StatusFail {
		return exitPartial
	}
	return exitOK
}

// stringList, tekrarlanabilir bir string bayrağı.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("boş canary")
	}
	*l = append(*l, v)
	return nil
}

func siteNames(cfgs []site.SiteConfig) []string {
	out := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, c.Name)
	}
	return out
}

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
		fmt.Fprintf(os.Stderr, "kullanım: siphon [bayraklar] [url ...]\n")
		fmt.Fprintf(os.Stderr, "          siphon doctor [bayraklar] [site ...]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	log := logger{verbose: verbose, quiet: quiet}

	// Ctrl+C: context iptal edilir, indirici .part'ı sync edip state yazar.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfgs, resolvers, err := setupResolvers(log, cfgPath, nil, nil)
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
		// bunkr 200 ile bakım placeholder'ı döndürebiliyor; durum kodu yeterli
		// sinyal değil.
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
