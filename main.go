// Siphon: pixeldrain ve bunkr linklerini toplu indirir.
//
// Bu dosya yalnızca komut satırı arayüzüdür: bayraklar, girdi okuma, çıktı
// biçimi. İndirme hattının tamamı internal/run'da ve pencere sürümü (cmd/
// siphon-gui) aynı hattı çağırıyor; iki arayüzün ayrışmaması bu ayrıma bağlı.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/mustafaberatdemirci/siphon/internal/config"
	"github.com/mustafaberatdemirci/siphon/internal/doctor"
	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
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

// events, logger'ı run.Events'e bağlar.
func (l logger) events() run.Events {
	return run.Events{
		Debugf: l.debugf,
		Infof:  l.infof,
		Errorf: l.errorf,
		ConfigLoaded: func(src string, n int) {
			l.debugf("config: %s (%d site)", src, n)
		},
		LedgerOpened: func(path string, items, _ int) {
			l.debugf("kayıt: %s (%d item)", path, items)
		},
		ItemResolved: func(it site.Item) {
			// --resolve-only çıktısı: sadece adres, satır satır. Boruya
			// verilebilir olması önemli.
			fmt.Fprintln(os.Stdout, it.URL)
		},
	}
}

func main() {
	// Alt komut, flag.Parse'tan ÖNCE ayrılıyor. Go'nun flag paketi ilk
	// bayrak olmayan argümanda duruyor, yani "siphon doctor -v" biçimini
	// tek bir FlagSet ile ayrıştırmak mümkün değil.
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(runDoctor(os.Args[2:]))
	}
	os.Exit(runCLI())
}

func runCLI() int {
	var (
		inputPath   string
		outDir      string
		cfgPath     string
		verbose     bool
		quiet       bool
		resolveOnly bool
		onQuota     string
	)
	flag.StringVar(&inputPath, "i", "", "URL listesi dosyası (satır başına bir URL, # ile yorum)")
	flag.StringVar(&outDir, "out", ".", "çıktı kökü")
	flag.StringVar(&cfgPath, "c", "", "sites.toml yolu (verilmezse exe yanı, sonra cwd, sonra gömülü)")
	flag.BoolVar(&verbose, "v", false, "katman detayını da bas")
	flag.BoolVar(&quiet, "q", false, "sadece hataları bas")
	flag.BoolVar(&resolveOnly, "resolve-only", false, "çözümlenen URL'leri bas, indirme")
	flag.StringVar(&onQuota, "on-quota", "", "site kotası dolunca çalıştırılacak komut (ör. VPN değiştiren betik); pay açılınca devam edilir")
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

	urls, err := readURLs(inputPath, flag.Args())
	if err != nil {
		log.errorf("%v", err)
		return run.ExitUsage
	}
	if len(urls) == 0 {
		log.errorf("girdi yok: -i ile dosya ver veya URL'leri argüman olarak geç")
		flag.Usage()
		return run.ExitUsage
	}

	sum, err := run.Run(ctx, run.Options{
		URLs:        urls,
		OutDir:      outDir,
		ConfigPath:  cfgPath,
		ResolveOnly: resolveOnly,
		OnQuota:     onQuota,
	}, log.events())
	if err != nil {
		log.errorf("%v", err)
		return run.ExitUsage
	}
	return sum.ExitCode()
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
		return run.ExitUsage
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

	cfgs, resolvers, err := run.Setup(log.events(), cfgPath, recFn, canaries)
	if err != nil {
		log.errorf("%v", err)
		return run.ExitUsage
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
		return run.ExitUsage
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
		return run.ExitPartial
	}
	return run.ExitOK
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
