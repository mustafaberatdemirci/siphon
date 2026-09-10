package site

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// BunkrName, registry kayıt anahtarı.
const BunkrName = "bunkr"

// Extra anahtarları ve varsayılanları.
//
// Bu üç değer bunkr'ın en çok değişen parçaları. 2025-02'de indirme adresi
// HTML'deki <source src> alanındaydı; 2025-03'te get.bunkrr.su/api/vs oldu;
// 2026 itibarıyla apidl.bunkr.ru/api/_001_v2. Üçü de config'te çünkü bir
// sonraki değişiklikte kod değiştirmek gerekmesin.
const (
	ExtraAPIEndpoint  = "api_endpoint"
	ExtraDLOrigin     = "dl_origin"
	ExtraXORPrefix    = "xor_key_prefix"
	ExtraSignEndpoint = "sign_endpoint"

	defaultBunkrAPIEndpoint = "https://dl.bunkr.cr/api/_001_v2"
	defaultBunkrDLOrigin    = "https://dl.bunkr.cr"
	defaultBunkrXORPrefix   = "SECRET_KEY_"

	// İmza servisi. CDN, imzasız isteği dosyaya hiç bakmadan 403 ile
	// reddediyor: var olan ve olmayan dosya için yanıt bayt bayt aynı.
	// 2026-09-10 ölçümü.
	defaultBunkrSignEndpoint = "https://glb-apisign.cdn.cr/sign"
)

// NewBunkr, registry'ye verilecek fabrikadır.
func NewBunkr(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	b := &bunkr{
		cfg:          cfg,
		apiEndpoint:  cfg.ExtraOr(ExtraAPIEndpoint, defaultBunkrAPIEndpoint),
		dlOrigin:     strings.TrimRight(cfg.ExtraOr(ExtraDLOrigin, defaultBunkrDLOrigin), "/"),
		xorPrefix:    cfg.ExtraOr(ExtraXORPrefix, defaultBunkrXORPrefix),
		signEndpoint: cfg.ExtraOr(ExtraSignEndpoint, defaultBunkrSignEndpoint),
		burned:       map[string]string{},
	}
	// Rotasyon havuzu SOMUT domainlerden kurulur; joker girdiler yalnızca
	// tanımaya yarar, rastgele seçilemez.
	for _, d := range cfg.Domains {
		if !strings.ContainsRune(d, '*') {
			b.active = append(b.active, normalizeHost(d))
		}
	}
	return b
}

type bunkr struct {
	cfg          SiteConfig
	apiEndpoint  string
	dlOrigin     string
	xorPrefix    string
	signEndpoint string

	// Rotasyon durumu. Global DEĞİL (gallery-dl'de paket seviyesinde bir küme);
	// resolver örneğine bağlı olması paralel testleri mümkün kılıyor.
	mu     sync.Mutex
	active []string
	burned map[string]string // domain -> yanma nedeni
}

// ---------- URL tanıma ----------

type bunkrKind int

const (
	bunkrAlbum bunkrKind = iota
	bunkrMedia
)

type bunkrRef struct {
	kind bunkrKind
	id   string // albüm id veya medya slug'ı
	seg  string // medya için yol ön eki: f, v, i, d
	host string
}

func (b *bunkr) Match(u string) bool {
	_, err := b.parse(u)
	return err == nil
}

func (b *bunkr) parse(raw string) (bunkrRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return bunkrRef{}, errors.New("boş URL")
	}
	if !strings.Contains(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bunkrRef{}, fmt.Errorf("URL ayrıştırılamadı: %w", err)
	}
	host := normalizeHost(u.Host)
	host = strings.TrimPrefix(host, "app.")

	known := MatchHost(host, b.cfg.Domains) ||
		MatchHost(host, b.cfg.LegacyDomains) ||
		MatchHost(host, b.cfg.MatchPatterns)
	if !known {
		return bunkrRef{}, fmt.Errorf("bilinmeyen host: %s", host)
	}

	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(seg) < 2 || seg[1] == "" {
		return bunkrRef{}, errors.New("yol eksik")
	}
	switch seg[0] {
	case "a":
		return bunkrRef{kind: bunkrAlbum, id: seg[1], host: host}, nil
	case "f", "v", "i", "d":
		return bunkrRef{kind: bunkrMedia, id: seg[1], seg: seg[0], host: host}, nil
	default:
		return bunkrRef{}, fmt.Errorf("desteklenmeyen yol: /%s", strings.Join(seg, "/"))
	}
}

// ---------- Domain rotasyonu ----------

// ErrAllDomainsBurned, tüm domainler elendiğinde döner.
var ErrAllDomainsBurned = errors.New("tüm bunkr domainleri elendi")

func (b *bunkr) roots() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.active))
	copy(out, b.active)
	return out
}

// burn, bir domaini rotasyon havuzundan çıkarır ve nedenini kaydeder.
// Neden kaydediliyor: doctor çıktısında "bunkr.cr operatör engeli, bunkr.black
// Cloudflare challenge" demek, "3 domain çalışmadı" demekten teşhis açısından
// apayrı bir şey.
func (b *bunkr) burn(domain, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, done := b.burned[domain]; done {
		return
	}
	b.burned[domain] = reason
	b.cfg.Logln("bunkr: %s elendi (%s); kalan domain: %d", domain, reason, len(b.active)-1)
	for i, d := range b.active {
		if d == domain {
			b.active = append(b.active[:i], b.active[i+1:]...)
			break
		}
	}
}

// Burned, elenen domainleri ve nedenlerini döndürür (doctor ve log için).
func (b *bunkr) Burned() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]string, len(b.burned))
	for k, v := range b.burned {
		out[k] = v
	}
	return out
}

// burnReason, bir hatanın domaini elemeyi gerektirip gerektirmediğini söyler.
//
// gallery-dl YALNIZCA 403'te eliyor. Ölçüm (2026-09-09) bunun eksik olduğunu
// gösterdi: Türkiye ağından bunkr domainlerinin çoğu SNI tabanlı engelli ve
// engel 403 olarak DEĞİL, sertifika doğrulama hatası veya bağlantı zaman aşımı
// olarak geliyor. Bu üçünü birlikte ele almak, rotasyonu hem Cloudflare hem
// operatör engeline karşı çalışır hale getiriyor.
func burnReason(err error) (string, bool) {
	var ch *challengeError
	if errors.As(err, &ch) {
		return "Cloudflare challenge (403)", true
	}

	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) {
		return "sertifika doğrulanamadı (operatör araya girmiş olabilir)", true
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return "TLS kaydı bozuk (araya giren kutu)", true
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS çözümlenemedi", true
	}

	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "bağlantı zaman aşımı (SNI filtresi olabilir)", true
	}

	// Bağlantı seviyesindeki her hata (reddedildi, erişilemiyor, sıfırlandı)
	// bu host'un kullanılamaz olduğu anlamına gelir. HTTP katmanına hiç
	// gelinemediği için içerikle ilgili bir şey söylenemez; tek doğru tepki
	// başka bir domain denemek.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "bağlantı kurulamadı: " + collapseSpace(opErr.Err.Error()), true
	}
	return "", false
}

// collapseSpace, çok satırlı sistem hata mesajlarını tek satıra indirir.
// Windows'un ağ hataları gömülü satır sonu içeriyor ve log çıktısını sarıyor;
// teşhis satırının tek satır kalması okunabilirlik için önemli.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// challengeError, 403'ü diğer HTTP hatalarından ayırır.
type challengeError struct {
	url  string
	body []byte
}

func (e *challengeError) Error() string { return "403: Cloudflare challenge: " + e.url }

// ---------- HTTP ----------

func (b *bunkr) client() *http.Client {
	if b.cfg.HTTPClient != nil {
		return b.cfg.HTTPClient
	}
	return http.DefaultClient
}

// get, tek bir adresi çeker. Yönlendirmeleri ELLE yönetir.
//
// Otomatik yönlendirme takibi kapalı, çünkü Cloudflare challenge'ı bir
// yönlendirme olarak da gelebiliyor ve otomatik takip o sinyali yutar.
// gallery-dl de aynı nedenle allow_redirects=False kullanıyor.
func (b *bunkr) get(ctx context.Context, rawURL, referer string) ([]byte, error) {
	const maxHops = 8
	cur := rawURL
	for hop := 0; hop < maxHops; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur, nil)
		if err != nil {
			return nil, Errorf(LayerFetch, cur, "istek kurulamadı: %v", err)
		}
		b.setHeaders(req, referer)

		resp, err := b.noRedirect().Do(req)
		if err != nil {
			return nil, unwrapURLError(err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if readErr != nil {
				return nil, Errorf(LayerFetch, cur, "gövde okunamadı: %v", readErr)
			}
			return body, nil

		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			loc := resp.Header.Get("Location")
			if loc == "" {
				return nil, Errorf(LayerFetch, cur, "%d ama Location yok", resp.StatusCode)
			}
			next, err := resolveLocation(cur, loc)
			if err != nil {
				return nil, Errorf(LayerFetch, cur, "Location çözülemedi: %v", err)
			}
			cur = next
			continue

		case resp.StatusCode == http.StatusForbidden:
			return nil, &challengeError{url: cur, body: body}

		default:
			return nil, Errorf(LayerFetch, cur, "HTTP %s", resp.Status)
		}
	}
	return nil, Errorf(LayerFetch, rawURL, "%d yönlendirmeden sonra vazgeçildi", maxHops)
}

// noRedirect, otomatik yönlendirme takibini kapatmış bir kopya döndürür.
func (b *bunkr) noRedirect() *http.Client {
	c := *b.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}

func (b *bunkr) setHeaders(req *http.Request, referer string) {
	if b.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", b.cfg.UserAgent)
	}
	switch b.cfg.RefererPolicy {
	case RefererItemPage:
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
	case RefererOrigin:
		req.Header.Set("Referer", "https://"+req.URL.Host+"/")
	}
}

func resolveLocation(base, loc string) (string, error) {
	bu, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	lu, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	return bu.ResolveReference(lu).String(), nil
}

// unwrapURLError, *url.Error sarmalını açar ki errors.As ile TLS/DNS türleri
// görülebilsin.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue
	}
	return err
}

// fetchWithRotation, path'i çalışan bir domain bulana kadar dener.
//
// Dönen ilk değer gövde, ikincisi isteğin gittiği root ("https://bunkr.ws").
// Root döndürülüyor çünkü sonraki istekler (item sayfası) aynı domainde
// kalmalı; her istekte baştan rotasyon denemek gereksiz gecikme üretir.
func (b *bunkr) fetchWithRotation(ctx context.Context, path, referer string) ([]byte, string, error) {
	roots := b.roots()
	if len(roots) == 0 {
		return nil, "", b.allBurnedError()
	}
	var last error
	for i, d := range roots {
		root := "https://" + d
		if i > 0 {
			b.cfg.Logln("bunkr: %s deneniyor (%d/%d)", d, i+1, len(roots))
		}
		body, err := b.get(ctx, root+path, referer)
		if err == nil {
			// Hangi domainin servis ettiği HER ZAMAN loglanıyor: "hangi domain
			// kullanıldı" sorusu rotasyonlu bir araçta ilk sorulan şey.
			b.cfg.Logln("bunkr: %s servis etti%s", d, ordinal(i))
			return body, root, nil
		}
		b.cfg.Logln("bunkr: %s başarısız: %s", d, collapseSpace(err.Error()))
		last = err
		if reason, ok := burnReason(err); ok {
			b.burn(d, reason)
			continue
		}
		// Kalıcı hata (404 gibi): domain sağlam, içerik yok. Rotasyon anlamsız.
		return nil, "", err
	}
	if len(b.roots()) == 0 {
		return nil, "", b.allBurnedError()
	}
	return nil, "", last
}

func (b *bunkr) allBurnedError() error {
	details := make([]string, 0, len(b.burned))
	for d, why := range b.Burned() {
		details = append(details, d+": "+why)
	}
	return &LayerError{
		Layer:    LayerChallenge,
		Err:      ErrAllDomainsBurned,
		Evidence: strings.Join(details, "; "),
	}
}

// ---------- Albüm ayrıştırma ----------

type bunkrFile struct {
	ID       string
	Name     string
	Slug     string
	Size     int64
	Ext      string
	MimeType string
}

// parseAlbumFiles, albüm sayfasındaki window.albumFiles dizisini ayrıştırır.
//
// Kaynak JSON DEĞİL, gömülü JavaScript: anahtarlar tırnaksız ve satır sonu
// virgüllü. gallery-dl bunu alan başına birebir ayırıcı eşleştirmesiyle
// okuyor (" id: " ve "size:  " gibi, çift boşluk dahil). Burada bilinçli
// olarak daha toleranslı bir yol seçildi: her satır "anahtar: değer,"
// biçiminde ayrıştırılıyor. Birebir boşluk eşleştirmesi tam olarak ilk
// bozulacak şey ve bu dosyanın var oluş nedeni o kırılmayı ucuzlatmak.
func parseAlbumFiles(page string) ([]bunkrFile, error) {
	const marker = "window.albumFiles"
	i := strings.Index(page, marker)
	if i < 0 {
		return nil, errors.New("window.albumFiles bulunamadı")
	}
	rest := page[i:]
	open := strings.Index(rest, "[")
	if open < 0 {
		return nil, errors.New("albumFiles dizisi açılmıyor")
	}
	end := strings.Index(rest, "</script>")
	if end < 0 || end < open {
		end = len(rest)
	}
	body := rest[open+1 : end]

	var out []bunkrFile
	for _, chunk := range splitJSObjects(body) {
		fields := parseJSFields(chunk)
		id := fields["id"]
		if id == "" {
			continue
		}
		f := bunkrFile{
			ID:       id,
			Name:     fields["original"],
			Slug:     fields["slug"],
			Ext:      fields["extension"],
			MimeType: fields["type"],
			Size:     -1,
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(fields["size"]), 10, 64); err == nil && n > 0 {
			f.Size = n
		}
		if f.Name == "" {
			// İsim yoksa slug + uzantı makul bir yedek; adsız item atlamaktan iyi.
			f.Name = strings.TrimSuffix(f.Slug, ".") + f.Ext
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, errors.New("albumFiles içinde item bulunamadı")
	}
	return out, nil
}

// splitJSObjects, "{...}," bloklarını süslü parantez dengesi üzerinden ayırır.
// Satır sonu kalıbına ("\n},\n") güvenmiyor: biçimlendirme değişirse o kalıp
// sessizce tek bir dev item üretir.
func splitJSObjects(body string) []string {
	var out []string
	depth, start := 0, -1
	inStr := false
	var quote rune
	esc := false
	for i, r := range body {
		if inStr {
			switch {
			case esc:
				esc = false
			case r == '\\':
				esc = true
			case r == quote:
				inStr = false
			}
			continue
		}
		switch r {
		case '"', '\'':
			inStr, quote = true, r
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, body[start:i+1])
				start = -1
			}
		}
	}
	return out
}

// parseJSFields, bir JS nesne bloğunu "anahtar -> değer" haritasına çevirir.
func parseJSFields(chunk string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "{")
		line = strings.TrimSuffix(line, "}")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.Trim(strings.TrimSpace(key), `"'`)
		val = strings.TrimSpace(val)
		val = strings.TrimSuffix(val, ",")
		val = strings.TrimSpace(val)
		if key == "" || val == "" {
			continue
		}
		out[key] = unquoteJS(val)
	}
	return out
}

// unquoteJS, tırnaklı bir JS değerini açar. JSON kaçışları varsa çözer,
// olmazsa tırnakları kırpmakla yetinir.
func unquoteJS(v string) string {
	if len(v) < 2 {
		return v
	}
	first, last := v[0], v[len(v)-1]
	if first == '"' && last == '"' {
		var s string
		if json.Unmarshal([]byte(v), &s) == nil {
			return s
		}
		return strings.Trim(v, `"`)
	}
	if first == '\'' && last == '\'' {
		inner := v[1 : len(v)-1]
		return strings.ReplaceAll(inner, `\'`, `'`)
	}
	return v
}

// ---------- API ve şifre çözme ----------

// bunkrAPIResponse, indirme API'sinin yanıtı. İKİ biçim de karşılanıyor.
//
// Güncel biçim (dl.bunkr.cr) adresi parçalı veriyor: mediafiles + path.
// Eski biçim (apidl.bunkr.ru) XOR ile şifrelenmiş tam adres veriyor ve
// 2026-09-10 ölçümünde BAYAT bir yol döndürüyordu: aynı dosya için
// ".../Castingcurvy---...m4v" derken güncel API ".../storage/media/..." diyor.
// Eski dal yalnızca geriye dönük uyumluluk için duruyor.
type bunkrAPIResponse struct {
	MediaFiles string `json:"mediafiles"`
	Path       string `json:"path"`
	Original   string `json:"original"`

	URL       string `json:"url"`
	Encrypted bool   `json:"encrypted"`
	Timestamp int64  `json:"timestamp"`
}

// bunkrSignResponse, imza servisinin yanıtı.
type bunkrSignResponse struct {
	Token string `json:"token"`
	Ex    int64  `json:"ex"`
}

// resolveFileURL, bir data id için gerçek indirme adresini çözer.
//
// Referer ve Origin ZORUNLU: endpoint bunları kontrol ediyor ve eksikse
// reddediyor. gallery-dl'in 2025-02-27 commit'i bu ikisini birlikte ekledi.
func (b *bunkr) resolveFileURL(ctx context.Context, dataID string) (string, string, error) {
	referer := b.dlOrigin + "/file/" + url.PathEscape(dataID)

	payload, err := json.Marshal(map[string]string{"id": dataID})
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiEndpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", "", Errorf(LayerItemPage, b.apiEndpoint, "istek kurulamadı: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", referer)
	req.Header.Set("Origin", b.dlOrigin)
	if b.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", b.cfg.UserAgent)
	}

	resp, err := b.client().Do(req)
	if err != nil {
		return "", "", &LayerError{Layer: LayerItemPage, Err: unwrapURLError(err), Evidence: b.apiEndpoint}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusBadRequest {
		// gallery-dl bu durumu "albüm silinmiş" olarak yorumluyor.
		return "", "", Errorf(LayerItemPage, dataID, "API 400: albüm veya dosya silinmiş olabilir")
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", Errorf(LayerItemPage, b.apiEndpoint, "API HTTP %s", resp.Status)
	}

	var data bunkrAPIResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return "", "", Errorf(LayerItemPage, b.apiEndpoint, "API yanıtı JSON değil: %v", err)
	}
	// Boşluk denetimi rawFileURL'de: güncel biçimde "url" alanı YOK, adres
	// mediafiles+path'ten kuruluyor. Burada url'e bakmak, çalışan yanıtı
	// hatalı saymak olurdu.
	rawURL, err := b.rawFileURL(data, dataID)
	if err != nil {
		return "", "", err
	}

	// İMZA ZORUNLU. İmzasız adres, dosya var olsa bile 403 dönüyor.
	signed, err := b.signURL(ctx, rawURL)
	if err != nil {
		return "", "", err
	}
	return signed, referer, nil
}

// rawFileURL, API yanıtından imzalanacak ham adresi kurar.
func (b *bunkr) rawFileURL(data bunkrAPIResponse, dataID string) (string, error) {
	if data.MediaFiles != "" && data.Path != "" {
		raw := strings.TrimRight(data.MediaFiles, "/") + data.Path
		if data.Original == "" {
			return raw, nil
		}
		// n, CDN'in Content-Disposition'da kullandığı özgün ad. Sitenin
		// kendisi de bunu imzadan ÖNCE ekliyor; imza yalnızca yola bakıyor,
		// bu yüzden sıralama sonucu değiştirmiyor.
		u, err := url.Parse(raw)
		if err != nil {
			return "", Errorf(LayerItemPage, dataID, "API adresi ayrıştırılamadı: %v", err)
		}
		q := u.Query()
		q.Set("n", data.Original)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}

	if data.URL == "" {
		return "", Errorf(LayerItemPage, dataID, "API ne mediafiles/path ne de url verdi")
	}
	if !data.Encrypted {
		return data.URL, nil
	}
	key := b.xorPrefix + strconv.FormatInt(data.Timestamp/3600, 10)
	dec, err := decryptXOR(data.URL, []byte(key))
	if err != nil {
		return "", Errorf(LayerItemPage, dataID, "URL şifresi çözülemedi: %v", err)
	}
	return dec, nil
}

// signURL, CDN adresini imza servisinden aldığı token ile imzalar.
//
// Neden ayrı bir servis: CDN, imzasız GET'i dosyaya bakmadan reddediyor.
// 2026-09-10 ölçümü: var olan dosya ile UYDURMA bir dosya adı için yanıt
// bayt bayt aynı (403, aynı gövde, aynı başlıklar). Bu yüzden 403'e bakıp
// "dosya silinmiş" demek YANLIŞ olurdu.
//
// Token süreli (ex alanı). Süresi dolduğunda indirici ResolveOne ile item'ı
// yeniden çözüyor; Item.SourcePage'in zorunlu olmasının gerekçesi bu.
func (b *bunkr) signURL(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", Errorf(LayerCDN, rawURL, "adres ayrıştırılamadı: %v", err)
	}

	// Servise yolun ÇÖZÜLMÜŞ hali gidiyor: sitenin JS'i decodeURIComponent
	// uygulayıp encodeURIComponent ile geri kodluyor. u.Path zaten çözülmüş
	// haldir, QueryEscape de "/" dahil her şeyi kodlar.
	endpoint := b.signEndpoint + "?path=" + url.QueryEscape(u.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", Errorf(LayerCDN, endpoint, "istek kurulamadı: %v", err)
	}
	if b.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", b.cfg.UserAgent)
	}

	resp, err := b.client().Do(req)
	if err != nil {
		return "", &LayerError{Layer: LayerCDN, Err: unwrapURLError(err), Evidence: endpoint}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	b.cfg.Recordln("sign", body)

	if resp.StatusCode != http.StatusOK {
		return "", Errorf(LayerCDN, endpoint, "imza servisi HTTP %s", resp.Status)
	}
	var sig bunkrSignResponse
	if err := json.Unmarshal(body, &sig); err != nil {
		return "", Errorf(LayerCDN, endpoint, "imza yanıtı JSON değil: %v", err)
	}
	if sig.Token == "" {
		return "", Errorf(LayerCDN, endpoint, "imza yanıtında token yok")
	}

	q := u.Query()
	q.Set("token", sig.Token)
	q.Set("ex", strconv.FormatInt(sig.Ex, 10))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// decryptXOR, base64 ile kodlanmış veriyi anahtarla XOR'layıp çözer.
//
// Anahtar zamana bağlı (timestamp/3600), yani çözülen URL saatlik pencerede
// geçerli. Bu, Item.SourcePage'in zorunlu olmasının bunkr tarafındaki
// gerekçesi: ertesi gün yapılan bir resume eski URL'de 403 alır ve item
// yeniden çözülmek zorundadır.
func decryptXOR(b64 string, key []byte) (string, error) {
	if len(key) == 0 {
		return "", errors.New("boş anahtar")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("base64 çözülemedi: %w", err)
	}
	out := make([]byte, len(raw))
	for i := range raw {
		out[i] = raw[i] ^ key[i%len(key)]
	}
	return string(out), nil
}

// ---------- Resolver arayüzü ----------

func (b *bunkr) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	ref, err := b.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}

	if ref.kind == bunkrMedia {
		item, err := b.resolveMedia(ctx, "/"+ref.seg+"/"+ref.id)
		if err != nil {
			return nil, err
		}
		if err := yield(item); err != nil {
			return nil, err
		}
		return nil, nil
	}

	// advanced=1 ZORUNLU: window.albumFiles yalnızca bu parametreyle geliyor.
	// Eski yol (grid-images_box div'lerini kazımak) 100 dosyadan sonrasını
	// kaçırıyordu; gallery-dl 2025-08-31'de tam bu yüzden veri kaynağını
	// değiştirdi. Yani bu bir sayfalama sorunu değil, kaynak sorunuydu.
	page, root, err := b.fetchWithRotation(ctx, "/a/"+url.PathEscape(ref.id)+"?advanced=1", "")
	if err != nil {
		return nil, err
	}
	text := string(page)

	files, err := parseAlbumFiles(text)
	if err != nil {
		return nil, Errorf(LayerParse, root+"/a/"+ref.id, "%v", err)
	}

	dir := sanitizeDirLabel(extractBetween(text, `property="og:title" content="`, `"`), ref.id)

	var itemErrs []ItemError
	for i, f := range files {
		fileURL, referer, ferr := b.resolveFileURL(ctx, f.ID)
		if ferr != nil {
			itemErrs = append(itemErrs, ItemError{
				URL: root + "/f/" + f.Slug,
				Err: ferr,
			})
			continue
		}
		item := Item{
			URL:        fileURL,
			SourcePage: root + "/f/" + f.Slug,
			Headers:    map[string]string{"Referer": referer},
			Dir:        dir,
			Filename:   f.Name,
			Size:       f.Size,
			Index:      i,
			// bunkr sha256 vermiyor; resume ETag/Last-Modified üzerinden yürür.
		}
		if err := yield(item); err != nil {
			return itemErrs, err
		}
	}
	return itemErrs, nil
}

// resolveMedia, tek bir medya sayfasından Item üretir.
func (b *bunkr) resolveMedia(ctx context.Context, path string) (Item, error) {
	page, root, err := b.fetchWithRotation(ctx, path, "")
	if err != nil {
		return Item{}, err
	}
	text := string(page)

	dataID := extractBetween(text, `data-file-id="`, `"`)
	if dataID == "" {
		return Item{}, Errorf(LayerParse, root+path, "data-file-id bulunamadı")
	}
	fileURL, referer, err := b.resolveFileURL(ctx, dataID)
	if err != nil {
		return Item{}, err
	}

	name := strings.TrimSpace(extractBetween(text, `property="og:title" content="`, `"`))
	if name == "" {
		name = strings.TrimPrefix(path[strings.LastIndex(path, "/"):], "/")
	}
	return Item{
		URL:        fileURL,
		SourcePage: root + path,
		Headers:    map[string]string{"Referer": referer},
		Filename:   name,
		Size:       -1,
		Index:      0,
	}, nil
}

// ResolveOne, imzalı URL süresi dolduğunda tek item'ı yeniden çözer.
// bunkr'da bu yol hayati: XOR anahtarı saatlik pencereye bağlı.
func (b *bunkr) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	ref, err := b.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if ref.kind != bunkrMedia {
		return Item{}, Errorf(LayerParse, sourcePage, "item sayfası bekleniyordu, albüm geldi")
	}
	return b.resolveMedia(ctx, "/"+ref.seg+"/"+ref.id)
}

// ClassifyStatus, indiricinin 403/410'u doğru yorumlamasını sağlar.
func (b *bunkr) ClassifyStatus(resp *http.Response, body []byte) error {
	if resp.StatusCode != http.StatusForbidden {
		return nil
	}
	if looksLikeChallenge(resp, body) {
		evidence := ""
		if resp.Request != nil && resp.Request.URL != nil {
			evidence = resp.Request.URL.String()
		}
		return &LayerError{
			Layer:    LayerChallenge,
			Err:      errors.New("CDN Cloudflare challenge döndürdü"),
			Evidence: evidence,
		}
	}
	// Challenge değilse imzalı URL'in süresi dolmuş olabilir; indirici
	// ResolveOne ile yeniden çözsün.
	return nil
}

// maintenanceNames, bunkr'ın bakım modunda servis ettiği placeholder dosyalar.
var maintenanceNames = []string{"/maint.mp4", "/maintenance-vid.mp4"}

// ValidateResponse, bakım placeholder'ını yakalar.
//
// bunkr silinen veya bakımdaki dosyalar için 404 DEĞİL, 200 ile bir
// placeholder video döndürüyor. Durum kodu temiz, içerik çöp. Bu kontrol
// olmadan araç çöpü "başarıyla indirdim" sayar; sessiz veri bozulmasının en
// kötü türü. gallery-dl aynı tespiti iki ayrı commit'te eklemek zorunda kaldı.
func (b *bunkr) ValidateResponse(resp *http.Response) error {
	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.Path
	}
	for _, name := range maintenanceNames {
		if strings.HasSuffix(final, name) {
			return Errorf(LayerCDN, final,
				"dosya sunucusu bakım modunda: placeholder video döndü, gerçek içerik değil")
		}
	}
	return nil
}

func (b *bunkr) Diagnose(ctx context.Context) ([]LayerResult, error) {
	if len(b.cfg.CanaryURLs) == 0 {
		return nil, errors.New("canary URL listesi boş")
	}
	var last []LayerResult
	for _, canary := range b.cfg.CanaryURLs {
		res := b.diagnoseOne(ctx, canary)
		last = res
		if !hasFail(res) {
			return res, nil
		}
	}
	return last, nil
}

func (b *bunkr) diagnoseOne(ctx context.Context, canary string) []LayerResult {
	out := make([]LayerResult, 0, 6)

	u, err := url.Parse(canary)
	if err != nil {
		return append(out, LayerResult{Layer: LayerDNS, Status: StatusFail,
			Detail: "canary URL ayrıştırılamadı", Evidence: canary})
	}
	host := normalizeHost(u.Host)

	addrs, dnsErr := net.DefaultResolver.LookupIPAddr(ctx, host)
	if dnsErr != nil {
		return append(out, LayerResult{Layer: LayerDNS, Status: StatusFail,
			Detail: "çözümlenemedi", Evidence: host + ": " + dnsErr.Error()})
	}
	ips := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP.String())
	}
	out = append(out, LayerResult{Layer: LayerDNS, Status: StatusOK,
		Detail: fmt.Sprintf("%d adres", len(ips)), Evidence: strings.Join(ips, ", ")})

	body, err := b.get(ctx, canary, "")
	// Yanıt her durumda kaydedilir: kırılan sayfanın gövdesi asıl kanıt, ama
	// çalışan gövde de gelecekteki diff'in referansı.
	b.cfg.Recordln("canary.html", body)

	switch {
	case err == nil:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "el sıkışma tamam"},
			LayerResult{Layer: LayerChallenge, Status: StatusOK, Detail: "challenge yok"},
			LayerResult{Layer: LayerFetch, Status: StatusOK,
				Detail: fmt.Sprintf("200, %d KB", len(body)/1024)},
		)
		// Parse katmanı yalnızca canary bir ALBÜM sayfasıysa doğrulanabilir.
		// Site kökü albumFiles taşımıyor; onu FAIL saymak doctor'ı yalancı
		// yapar ve "parse kırıldı" diye yanlış yöne gönderir.
		if !strings.Contains(u.Path, "/a/") {
			out = append(out,
				LayerResult{Layer: LayerParse, Status: StatusWarn,
					Detail:   "albüm canary'si yok, ayrıştırma doğrulanmadı",
					Evidence: "canary_urls'e bir /a/<id> adresi ekle"},
				LayerResult{Layer: LayerItemPage, Status: StatusWarn,
					Detail: "albüm canary'si yok, API zinciri doğrulanmadı"},
				LayerResult{Layer: LayerCDN, Status: StatusWarn,
					Detail: "albüm canary'si yok, CDN host'u görülmedi"},
			)
		} else if files, perr := parseAlbumFiles(string(body)); perr != nil {
			out = append(out, LayerResult{Layer: LayerParse, Status: StatusFail,
				Detail: "albumFiles ayrıştırılamadı", Evidence: perr.Error()})
		} else {
			out = append(out, LayerResult{Layer: LayerParse, Status: StatusOK,
				Detail: fmt.Sprintf("albumFiles %d item buldu", len(files))})
			out = append(out, b.diagnoseItemAndCDN(ctx, files)...)
		}

	default:
		reason, burnable := burnReason(err)
		layer := LayerFetch
		if l, ok := LayerOf(err); ok {
			layer = l
		}
		var ch *challengeError
		switch {
		case errors.As(err, &ch):
			layer = LayerChallenge
		case burnable && strings.Contains(reason, "sertifika"):
			layer = LayerTLS
		case burnable && strings.Contains(reason, "zaman aşımı"):
			layer = LayerTLS
		}
		detail := "istek başarısız"
		if burnable {
			detail = reason
		}
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "DNS geçti"},
			LayerResult{Layer: layer, Status: StatusFail, Detail: detail, Evidence: err.Error()},
		)
	}
	return out
}

// diagnoseItemAndCDN, albüm canary'si varsa API zincirini ve CDN host'unu
// doğrular.
//
// TEK item çözülür. doctor bir teşhis aracı; 200 item için 200 API çağrısı
// atmak teşhisi cezaya çevirir ve rate limit'i kendi elinle tetikler.
func (b *bunkr) diagnoseItemAndCDN(ctx context.Context, files []bunkrFile) []LayerResult {
	var out []LayerResult

	fileURL, _, err := b.resolveFileURL(ctx, files[0].ID)
	if err != nil {
		return append(out,
			LayerResult{Layer: LayerItemPage, Status: StatusFail,
				Detail: "API zinciri kırıldı", Evidence: collapseSpace(err.Error())},
			LayerResult{Layer: LayerCDN, Status: StatusWarn,
				Detail: "ItemPage kırıldığı için CDN host'u görülemedi"},
		)
	}
	out = append(out, LayerResult{Layer: LayerItemPage, Status: StatusOK,
		Detail: fmt.Sprintf("1/%d item çözüldü (örnekleme)", len(files))})

	host := ""
	if u, perr := url.Parse(fileURL); perr == nil {
		host = u.Host
	}
	switch {
	case host == "":
		out = append(out, LayerResult{Layer: LayerCDN, Status: StatusFail,
			Detail: "çözülen adreste host yok", Evidence: fileURL})
	case MatchHost(host, b.cfg.CDNPatterns):
		out = append(out, LayerResult{Layer: LayerCDN, Status: StatusOK,
			Detail: "bilinen CDN host'u", Evidence: host})
	default:
		// CDN bir KAPI DEĞİL, SİNYAL. Bilinmeyen host indirmeyi durdurmaz;
		// bunkr host'ları normal işleyişte dönüyor. Kapı yapmak, aracın sonra
		// teşhis edeceği kırılmayı bizzat üretmek olurdu.
		out = append(out, LayerResult{Layer: LayerCDN, Status: StatusWarn,
			Detail:   "yeni CDN host'u, cdn_patterns'da yok",
			Evidence: host + " (sites.toml'a ekle)"})
	}
	return out
}

// extractBetween, ilk start...end arasını döndürür; bulunamazsa "".
func extractBetween(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return ""
	}
	return s[:j]
}

// ordinal, kaçıncı denemede başarılı olunduğunu log için biçimlendirir.
func ordinal(i int) string {
	if i == 0 {
		return " (ilk deneme)"
	}
	return fmt.Sprintf(" (%d. deneme)", i+1)
}
