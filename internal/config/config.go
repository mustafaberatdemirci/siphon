// Package config, sites.toml'un yüklenmesi ve birleştirilmesinden sorumludur.
//
// Dosya düzeni dokümanında ayrı bir config paketi yoktu; birleştirme mantığı
// main.go'ya bırakılmıştı. Ayrı paket olmasının gerekçesi: union merge,
// domains_remove ve schema_version doğrulaması tablo testi isteyen gerçek
// mantık, ve bunları bayrak ayrıştırmasıyla aynı dosyada tutmak ikisini de
// zorlaştırıyor.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// SchemaVersion, bu binary'nin anladığı şema sürümü.
const SchemaVersion = 1

// ErrUsage, çıkış kodu 3 ile eşleşen hata sınıfıdır: geçersiz TOML,
// schema_version uyuşmazlığı, okunamayan dosya. "Site tarafında bir şey oldu"
// (çıkış 2) ile karıştırılmaz; teşhis yolları tamamen farklı.
var ErrUsage = errors.New("konfigürasyon hatası")

type file struct {
	SchemaVersion *int      `toml:"schema_version"`
	Sites         []rawSite `toml:"site"`
}

type rawSite struct {
	Name string `toml:"name"`

	Domains       []string `toml:"domains"`
	LegacyDomains []string `toml:"legacy_domains"`
	MatchPatterns []string `toml:"match_patterns"`
	CDNPatterns   []string `toml:"cdn_patterns"`
	CanaryURLs    []string `toml:"canary_urls"`

	// Silme açık olmak zorunda. Dizi alanları birleştiği için, bir domain'i
	// listeden çıkarmanın başka yolu yok.
	DomainsRemove     []string `toml:"domains_remove"`
	CDNPatternsRemove []string `toml:"cdn_patterns_remove"`

	UserAgent     string `toml:"user_agent"`
	RefererPolicy string `toml:"referer_policy"`
	MaxConcurrent int    `toml:"max_concurrent"`
	MaxSegments   int    `toml:"max_segments"`
	DNSResolver   string `toml:"dns_resolver"`

	MaxRetries int    `toml:"max_retries"`
	BaseDelay  string `toml:"base_delay"`
	MaxDelay   string `toml:"max_delay"`
	MaxElapsed string `toml:"max_elapsed"`

	// Extra, siteye özgü anahtar/değer ayarları (bunkr'ın api_endpoint'i gibi).
	Extra map[string]string `toml:"extra"`
}

// Source, konfigürasyonun nereden geldiğini söyler. -v çıktısında basılır;
// "hangi config yüklendi" sorusu teşhisin ilk adımı.
type Source struct {
	Path     string // boşsa gömülü kopya
	Embedded bool
}

func (s Source) String() string {
	if s.Embedded {
		return "gömülü"
	}
	return s.Path
}

// Load, gömülü kopyayı okur ve bulunursa dış kopyayı üzerine bindirir.
//
// Arama sırası: explicitPath -> exe'nin yanı -> cwd. İlk bulunan kullanılır.
// explicitPath verilmiş ama dosya yoksa bu bir kullanım hatasıdır; sessizce
// gömülüye düşmek teşhisi imkânsızlaştırır.
func Load(embedded []byte, explicitPath string) ([]site.SiteConfig, Source, error) {
	base, err := parse(embedded, "gömülü sites.toml")
	if err != nil {
		return nil, Source{}, err
	}
	if base.SchemaVersion == nil {
		return nil, Source{}, fmt.Errorf("%w: gömülü sites.toml'da schema_version yok", ErrUsage)
	}
	if *base.SchemaVersion != SchemaVersion {
		return nil, Source{}, fmt.Errorf("%w: gömülü schema_version=%d, bu binary %d bekliyor",
			ErrUsage, *base.SchemaVersion, SchemaVersion)
	}

	path, err := locate(explicitPath)
	if err != nil {
		return nil, Source{}, err
	}
	if path == "" {
		cfgs, err := build(base)
		return cfgs, Source{Embedded: true}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Source{}, fmt.Errorf("%w: %s okunamadı: %v", ErrUsage, path, err)
	}
	ext, err := parse(data, path)
	if err != nil {
		return nil, Source{}, err
	}
	// Dış dosyada schema_version opsiyonel; verilmişse uyuşmak zorunda.
	if ext.SchemaVersion != nil && *ext.SchemaVersion != SchemaVersion {
		return nil, Source{}, fmt.Errorf("%w: %s schema_version=%d, bu binary %d bekliyor",
			ErrUsage, path, *ext.SchemaVersion, SchemaVersion)
	}

	cfgs, err := build(merge(base, ext))
	return cfgs, Source{Path: path}, err
}

// locate, dış config dosyasını arar. Bulunamazsa boş yol döner (gömülü kullanılır).
func locate(explicitPath string) (string, error) {
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err != nil {
			return "", fmt.Errorf("%w: -c ile verilen %s açılamadı: %v", ErrUsage, explicitPath, err)
		}
		return explicitPath, nil
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "sites.toml"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "sites.toml"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", nil
}

func parse(data []byte, label string) (file, error) {
	var f file
	// Geçersiz TOML hard fail: sessizce gömülüye dönmek, aracın tüm amacı olan
	// teşhis edilebilirliği yok eder.
	if _, err := toml.Decode(string(data), &f); err != nil {
		return file{}, fmt.Errorf("%w: %s geçersiz TOML: %v", ErrUsage, label, err)
	}
	return f, nil
}

// merge, dış dosyayı temel üzerine bindirir.
func merge(base, ext file) file {
	out := file{SchemaVersion: base.SchemaVersion}
	out.Sites = append(out.Sites, base.Sites...)

	for _, e := range ext.Sites {
		idx := -1
		for i, b := range out.Sites {
			if b.Name == e.Name {
				idx = i
				break
			}
		}
		if idx < 0 {
			out.Sites = append(out.Sites, e)
			continue
		}
		out.Sites[idx] = mergeSite(out.Sites[idx], e)
	}
	return out
}

func mergeSite(b, e rawSite) rawSite {
	// Diziler birleşir. Silme burada DEĞİL build()'de uygulanıyor: aksi halde
	// dış dosyanın yeni tanımladığı bir sitenin kendi domains_remove'u hiç
	// çalışmazdı (o dal merge'e girmiyor).
	b.Domains = union(b.Domains, e.Domains)
	b.CDNPatterns = union(b.CDNPatterns, e.CDNPatterns)
	b.LegacyDomains = union(b.LegacyDomains, e.LegacyDomains)
	b.MatchPatterns = union(b.MatchPatterns, e.MatchPatterns)
	b.CanaryURLs = union(b.CanaryURLs, e.CanaryURLs)

	// Extra anahtar bazında eziliyor: dış dosya yalnızca değiştirmek istediği
	// anahtarı yazsın, tüm haritayı yeniden yazmak zorunda kalmasın.
	if len(e.Extra) > 0 {
		if b.Extra == nil {
			b.Extra = map[string]string{}
		}
		for k, v := range e.Extra {
			b.Extra[k] = v
		}
	}
	b.DomainsRemove = union(b.DomainsRemove, e.DomainsRemove)
	b.CDNPatternsRemove = union(b.CDNPatternsRemove, e.CDNPatternsRemove)

	// Skalerler ezilir, ama yalnızca verilmişlerse.
	if e.UserAgent != "" {
		b.UserAgent = e.UserAgent
	}
	if e.RefererPolicy != "" {
		b.RefererPolicy = e.RefererPolicy
	}
	if e.DNSResolver != "" {
		b.DNSResolver = e.DNSResolver
	}
	if e.MaxConcurrent != 0 {
		b.MaxConcurrent = e.MaxConcurrent
	}
	if e.MaxSegments != 0 {
		b.MaxSegments = e.MaxSegments
	}
	if e.MaxRetries != 0 {
		b.MaxRetries = e.MaxRetries
	}
	if e.BaseDelay != "" {
		b.BaseDelay = e.BaseDelay
	}
	if e.MaxDelay != "" {
		b.MaxDelay = e.MaxDelay
	}
	if e.MaxElapsed != "" {
		b.MaxElapsed = e.MaxElapsed
	}
	return b
}

// union, sıra koruyarak birleştirir ve yinelenenleri atar.
func union(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// remove, silme listesini uygular.
//
// Karşılaştırma NORMALİZE edilerek yapılır: eşleştirme tarafı (site.MatchHost)
// host'u küçük harfe çeviriyor ve sondaki noktayı atıyor. Burada ham string
// karşılaştırmak, `domains_remove = ["PixelDrain.COM"]` yazan bir kullanıcının
// silmesinin sessizce hiçbir şey yapmaması demek olurdu.
func remove(from, drop []string) []string {
	if len(drop) == 0 {
		return from
	}
	norm := func(s string) string {
		return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	}
	bad := make(map[string]bool, len(drop))
	for _, d := range drop {
		bad[norm(d)] = true
	}
	out := make([]string, 0, len(from))
	for _, s := range from {
		if !bad[norm(s)] {
			out = append(out, s)
		}
	}
	return out
}

func build(f file) ([]site.SiteConfig, error) {
	if len(f.Sites) == 0 {
		return nil, fmt.Errorf("%w: hiç site tanımı yok", ErrUsage)
	}
	out := make([]site.SiteConfig, 0, len(f.Sites))
	seen := map[string]bool{}
	for _, r := range f.Sites {
		if r.Name == "" {
			return nil, fmt.Errorf("%w: adı olmayan site tanımı", ErrUsage)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("%w: %q iki kez tanımlanmış", ErrUsage, r.Name)
		}
		seen[r.Name] = true

		switch r.RefererPolicy {
		case "", site.RefererNone, site.RefererItemPage, site.RefererOrigin:
		default:
			return nil, fmt.Errorf("%w: %s: geçersiz referer_policy %q (none|item_page|origin)",
				ErrUsage, r.Name, r.RefererPolicy)
		}
		// Silme burada uygulanıyor: hem birleştirilmiş hem dış dosyanın yeni
		// tanımladığı siteler aynı yoldan geçsin.
		domains := remove(r.Domains, r.DomainsRemove)
		cdn := remove(r.CDNPatterns, r.CDNPatternsRemove)

		if len(r.Domains) == 0 {
			return nil, fmt.Errorf("%w: %s: domains boş", ErrUsage, r.Name)
		}
		if len(domains) == 0 {
			return nil, fmt.Errorf("%w: %s: domains_remove tüm domainleri sildi", ErrUsage, r.Name)
		}

		base, err := dur(r.Name, "base_delay", r.BaseDelay)
		if err != nil {
			return nil, err
		}
		maxD, err := dur(r.Name, "max_delay", r.MaxDelay)
		if err != nil {
			return nil, err
		}
		elapsed, err := dur(r.Name, "max_elapsed", r.MaxElapsed)
		if err != nil {
			return nil, err
		}

		out = append(out, site.SiteConfig{
			Name:          r.Name,
			Domains:       domains,
			LegacyDomains: r.LegacyDomains,
			MatchPatterns: r.MatchPatterns,
			CDNPatterns:   cdn,
			Extra:         r.Extra,
			CanaryURLs:    r.CanaryURLs,
			UserAgent:     r.UserAgent,
			RefererPolicy: r.RefererPolicy,
			MaxConcurrent: r.MaxConcurrent,
			MaxSegments:   r.MaxSegments,
			DNSResolver:   r.DNSResolver,
			MaxRetries:    r.MaxRetries,
			BaseDelay:     base,
			MaxDelay:      maxD,
			MaxElapsed:    elapsed,
		}.WithDefaults())
	}
	return out, nil
}

func dur(siteName, field, v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %s=%q ayrıştırılamadı: %v", ErrUsage, siteName, field, v, err)
	}
	return d, nil
}
