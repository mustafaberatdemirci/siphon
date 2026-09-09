// Package site, Siphon'un çekirdek sözleşmesini tanımlar: bir siteden Item üretmek
// ve kırılma halinde hangi katmanın koptuğunu tipli olarak bildirmek.
//
// Manşet özellik katman raporu olduğu için resolver hataları düz error dönmez;
// her hata bir Layer'a bağlanır ve doctor bunu errors.As ile okur.
package site

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Layer, bir çözümleme denemesinin kırılabileceği aşamalardır.
//
// DNS, TLS ve Challenge kasıtlı olarak AYRI katmanlardır. 2026-09-09 ölçümü
// üçünün de farklı hata imzası ürettiğini ve zıt düzeltmeler gerektirdiğini
// gösterdi:
//
//	DNS       : çözümlenen adres beklenen ağın dışında -> operatör engeli,
//	            çözüm domain değiştirmek. (bunkr.cr -> 2a01:358:... blok sayfası)
//	TLS       : el sıkışma veya sertifika doğrulaması başarısız -> aynı engelin
//	            TLS katmanındaki görünümü (x509: unknown authority).
//	Challenge : TLS temiz ama 403 + CF-Mitigated: challenge -> Cloudflare,
//	            çözüm önce domain rotasyonu, son çare utls.
//
// Bu üçünü tek katmanda toplamak doctor'ı yalancı yapar: kullanıcıya "TLS sorunu"
// der ama gereken şey domain değiştirmektir.
type Layer string

const (
	LayerDNS       Layer = "DNS"
	LayerTLS       Layer = "TLS"
	LayerChallenge Layer = "Challenge"
	LayerFetch     Layer = "Fetch"
	LayerParse     Layer = "Parse"
	LayerItemPage  Layer = "ItemPage"
	LayerCDN       Layer = "CDN"
)

// Layers, doctor'ın raporlama sırası.
var Layers = []Layer{
	LayerDNS, LayerTLS, LayerChallenge,
	LayerFetch, LayerParse, LayerItemPage, LayerCDN,
}

// LayerError, bir hatayı katmana bağlar. Evidence teşhis için ham kanıttır.
type LayerError struct {
	Layer    Layer
	Err      error
	Evidence string // "beklenen host kalıbı eşleşmedi: kirsch-cdn.ru"
}

func (e *LayerError) Error() string {
	if e.Evidence == "" {
		return fmt.Sprintf("%s: %v", e.Layer, e.Err)
	}
	return fmt.Sprintf("%s: %v (%s)", e.Layer, e.Err, e.Evidence)
}

func (e *LayerError) Unwrap() error { return e.Err }

// Errorf, katmana bağlı hata üretmenin kısa yolu.
func Errorf(l Layer, evidence string, format string, a ...any) *LayerError {
	return &LayerError{Layer: l, Err: fmt.Errorf(format, a...), Evidence: evidence}
}

// LayerOf, zincirdeki ilk LayerError'ın katmanını döndürür.
func LayerOf(err error) (Layer, bool) {
	var le *LayerError
	if errors.As(err, &le) {
		return le.Layer, true
	}
	return "", false
}

type LayerStatus string

const (
	StatusOK   LayerStatus = "OK"
	StatusWarn LayerStatus = "WARN" // örn. sites.toml'da olmayan yeni CDN host'u
	StatusFail LayerStatus = "FAIL"
)

// LayerResult, doctor'ın bastığı satır. WARN ile FAIL ayrımı olmadan
// "CDN gate değil sinyal" kuralı ifade edilemez.
type LayerResult struct {
	Layer    Layer
	Status   LayerStatus
	Detail   string // "albüm seçicisi 12 item buldu"
	Evidence string // "yeni host görüldü: kirsch-cdn.ru"
}

type Item struct {
	URL        string // indirilecek gerçek URL (CDN, süreli olabilir)
	SourcePage string // yeniden çözümleme için item sayfası. Zorunlu.
	Dir        string // çıktı köküne göreli albüm klasörü ("" = kök)
	Filename   string
	Headers    map[string]string // Referer dahil, RefererPolicy'den türetilir
	SHA256     string            // pixeldrain verir, bunkr vermez, boş olabilir
	Size       int64             // bilinmiyorsa -1
	Index      int               // albüm içi sıra
}

type ItemError struct {
	URL string
	Err error // LayerError sarabilir
}

func (e *ItemError) Error() string { return fmt.Sprintf("%s: %v", e.URL, e.Err) }
func (e *ItemError) Unwrap() error { return e.Err }

// Referer politikası site başına değişir, tek tip mekanizma değildir.
// pixeldrain'de yanlış Referer tam olarak hotlink_detected tetikler;
// bunkr'da item sayfası Referer olarak zorunludur.
const (
	RefererNone     = "none"
	RefererItemPage = "item_page"
	RefererOrigin   = "origin"
)

// SiteConfig varsayılanları.
const (
	DefaultMaxRetries    = 5
	DefaultBaseDelay     = 1 * time.Second
	DefaultMaxDelay      = 60 * time.Second
	DefaultMaxElapsed    = 10 * time.Minute
	DefaultMaxConcurrent = 2
)

type SiteConfig struct {
	Name string

	// Domains, eşleşme için kullanılan aktif domain havuzudur. Girdiler joker
	// içerebilir ("bunkr.*"). gallery-dl 2024-08-24'te TLD saymaktan vazgeçip
	// joker seçeneği ekledi; bunkr tarafında liste tutmak kaybedilmiş bir savaş.
	Domains []string

	// LegacyDomains, URL eşleşmesinde KABUL edilir ama fetch için KULLANILMAZ.
	// gallery-dl aynı ayrımı LEGACY_DOMAINS ile yapıyor: ölü bir domain'den gelen
	// linki tanımak gerekir, o domain'e istek atmak gerekmez.
	LegacyDomains []string

	// MatchPatterns, yalnızca TANIMA için joker desenler ("bunkr.*").
	// LegacyDomains ile aynı semantik: eşleşmeyi sağlar, fetch havuzuna girmez.
	//
	// Neden ayrı: domain rotasyonu somut bir listeye ihtiyaç duyuyor (jokerle
	// rastgele bir domain seçemezsin), ama liste her zaman bayat olacak. İkisi
	// birlikte: yeni bir TLD çıktığında link tanınır, istek bilinen bir domaine
	// gider. gallery-dl de aynı ikiliyi kullanıyor (BASE_PATTERN + DOMAINS).
	MatchPatterns []string

	CDNPatterns   []string // gate değil, sinyal
	UserAgent     string
	RefererPolicy string // none | item_page | origin
	MaxConcurrent int

	// CanaryURLs bir LİSTEDİR, tek URL değil. Ölçüm, bunkr.cr'nin bu ağda
	// operatör tarafından engellendiğini gösterdi; tek canary'ye bağlanan bir
	// doctor, çalışır durumdaki siteyi "ölü" diye raporlardı. doctor çalışan
	// ilkini bulana kadar dener.
	CanaryURLs []string

	// DNSResolver boş ise sistem çözümleyicisi kullanılır; aksi halde bir DoH
	// adresi. SNI tabanlı engeli ÇÖZMEZ, sadece DNS hijack'ini çözer. Asıl
	// değeri teşhiste: sistem DNS'i ile DoH farklı cevap veriyorsa bu tek
	// başına operatör müdahalesinin kanıtıdır.
	DNSResolver string

	// Retry politikası config'te, çünkü Premise 2 bunu vaat ediyor.
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	MaxElapsed time.Duration // item başına

	// Extra, siteye özgü ayarlardır (bunkr'ın API endpoint'i gibi).
	//
	// Paylaşılan SiteConfig'e site'a özgü alan eklemek yerine burada
	// tutuluyor: bunkr'ın endpoint'i üç kez değişmiş bir değer ve tam olarak
	// "config = değişkenler" kategorisine giriyor, ama pixeldrain'i
	// ilgilendirmiyor.
	Extra map[string]string

	// Record, --record ile etkinleşen yanıt kaydedicisi. nil olabilir.
	//
	// Neden var: bunkr kırıldığında elinde kırılan sayfanın GERÇEK yanıtı
	// olmazsa, eski fixture ile diff alamazsın ve neyin değiştiğini tahminle
	// kovalarsın. Bu aracın var oluş gerekçesi o tahmini ortadan kaldırmak.
	Record func(name string, data []byte)

	// Logf, resolver'ın teşhis satırları için. nil olabilir.
	//
	// Config'e bir logger koymak ilk bakışta yersiz duruyor, ama bu aracın
	// manşet özelliği teşhis edilebilirlik: domain rotasyonu sessizce olursa
	// kullanıcı "neden yavaş" veya "neden başka bir domaine gitti" sorusunu
	// cevaplayamaz. HTTPClient de aynı gerekçeyle burada.
	Logf func(format string, a ...any)

	// Test enjeksiyonunun TEK mekanizması. httptest.Server'a yönlendirme,
	// bu client'a takılan rewriting RoundTripper ile yapılır.
	HTTPClient *http.Client
}

// Logln, cfg.Logf varsa yazar.
func (c SiteConfig) Logln(format string, a ...any) {
	if c.Logf != nil {
		c.Logf(format, a...)
	}
}

// Recordln, cfg.Record varsa yanıtı kaydedir.
func (c SiteConfig) Recordln(name string, data []byte) {
	if c.Record != nil && len(data) > 0 {
		c.Record(name, data)
	}
}

// ExtraOr, Extra'dan bir değer okur; yoksa varsayılanı döndürür.
func (c SiteConfig) ExtraOr(key, def string) string {
	if v, ok := c.Extra[key]; ok && v != "" {
		return v
	}
	return def
}

// WithDefaults, sıfır değerli alanları varsayılanlarıyla doldurur.
func (c SiteConfig) WithDefaults() SiteConfig {
	if c.MaxRetries == 0 {
		c.MaxRetries = DefaultMaxRetries
	}
	if c.BaseDelay == 0 {
		c.BaseDelay = DefaultBaseDelay
	}
	if c.MaxDelay == 0 {
		c.MaxDelay = DefaultMaxDelay
	}
	if c.MaxElapsed == 0 {
		c.MaxElapsed = DefaultMaxElapsed
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = DefaultMaxConcurrent
	}
	if c.RefererPolicy == "" {
		c.RefererPolicy = RefererNone
	}
	return c
}

type Resolver interface {
	Match(u string) bool

	// Resolve, her item bulundukça yield çağırır. Kısmi başarı ifade edilebilir:
	// "900 çözüldü, 100 hata" durumu []ItemError ile döndürülür.
	Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error)

	// ResolveOne, imzalı CDN URL'i 403/410 aldığında tek item'ı yeniden çözer.
	// Premise 4 buna bağlı. sourcePage, Item.SourcePage ile aynı değerdir.
	ResolveOne(ctx context.Context, sourcePage string) (Item, error)

	// Diagnose, canary üzerinde katman raporu üretir.
	Diagnose(ctx context.Context) ([]LayerResult, error)
}

// StatusClassifier, bir resolver'ın HTTP hata durumlarını siteye özgü biçimde
// sınıflandırmasını sağlar. İndirici bunu opsiyonel olarak kullanır (type
// assertion ile), bu yüzden Resolver arayüzünü genişletmiyor.
//
// Neden gerekli: indirici tek başına 403'ü "imzalı URL süresi doldu" sayar.
// pixeldrain ise rate limit, hotlink ve captcha durumlarını da 403 ile
// bildiriyor. Bu ayrım yapılmazsa araç rate limitliyken URL'i yeniden çözüp
// tekrar dener, yani limiti kendi eliyle derinleştirir.
//
// nil dönmek "tanımadım, varsayılanı uygula" demektir.
type StatusClassifier interface {
	ClassifyStatus(resp *http.Response, body []byte) error
}

// ResponseValidator, bir resolver'ın indirme yanıtını siteye özgü biçimde
// doğrulamasını sağlar. İndirici bunu opsiyonel olarak kullanır.
//
// Neden gerekli: bunkr silinen veya bakımdaki dosyalar için 404 yerine 200 ile
// bir placeholder video servis ediyor (maint.mp4). Durum kodu temiz, içerik
// çöp. Bu kanca olmadan araç çöpü "başarıyla indirdim" sayar ve bu, sessiz
// veri bozulmasının en kötü türü.
//
// nil dönmek "yanıt geçerli" demektir.
type ResponseValidator interface {
	ValidateResponse(resp *http.Response) error
}

// Factory, config'i resolver'a enjekte eder.
type Factory func(cfg SiteConfig) Resolver

// Registry global durum tutmaz; her test kendi örneğini kurar.
type Registry struct {
	factories map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

func (r *Registry) Register(name string, f Factory) error {
	if name == "" {
		return errors.New("site: boş isimle kayıt")
	}
	if f == nil {
		return fmt.Errorf("site: %q için nil factory", name)
	}
	if _, dup := r.factories[name]; dup {
		return fmt.Errorf("site: %q zaten kayıtlı", name)
	}
	r.factories[name] = f
	return nil
}

// Build, her SiteConfig için kayıtlı fabrikayı çağırır ve resolver listesi üretir.
func (r *Registry) Build(cfgs []SiteConfig) ([]Resolver, error) {
	out := make([]Resolver, 0, len(cfgs))
	for _, cfg := range cfgs {
		f, ok := r.factories[cfg.Name]
		if !ok {
			return nil, fmt.Errorf("site: %q için kayıtlı factory yok", cfg.Name)
		}
		out = append(out, f(cfg.WithDefaults()))
	}
	return out, nil
}
