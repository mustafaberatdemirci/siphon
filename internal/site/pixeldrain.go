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
	"strings"
)

// PixeldrainName, registry kayıt anahtarı.
const PixeldrainName = "pixeldrain"

// NewPixeldrain, registry'ye verilecek fabrikadır.
// ExtraPixeldrainAPIKey, sites.toml'daki [site.extra] api_key anahtarı.
//
// pixeldrain'in kendi belgesi: "Hotlinking is only allowed when either the
// uploader or the downloader has a premium subscription." Üçüncü parti bir
// indirici, tanım gereği hotlink. Bir dosyanın indirme sayısı görüntülenme
// sayısının üç katını aşınca captcha kapısı iniyor ve bu DOSYA BAŞINA bir
// sayaç: IP değiştirmek (VPN) hiçbir şey değiştirmiyor.
//
// Tasarlanmış çıkış yolu ücretli hesabın API anahtarı. HTTP Basic ile
// gönderiliyor: kullanıcı adı boş, parola anahtar. Hem API çağrılarına hem
// TRANSFER isteklerine ekleniyor; sınır asıl transferde biniyor.
//
// Captcha'yı çözmeye veya görüntülenme sayısını şişirmeye ÇALIŞMIYORUZ:
// birincisi kapsam dışı, ikincisi sitenin erişim kontrolünü kandırmak.
const ExtraPixeldrainAPIKey = "api_key"

func NewPixeldrain(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	return &pixeldrain{
		cfg:    cfg,
		apiKey: strings.TrimSpace(cfg.ExtraOr(ExtraPixeldrainAPIKey, "")),
	}
}

type pixeldrain struct {
	cfg    SiteConfig
	apiKey string // boşsa anonim
}

// authHeader, API anahtarı varsa HTTP Basic başlığının değerini üretir.
// Anahtar log'a, kayda veya hata kanıtına ASLA yazılmıyor; yalnızca başlık.
func (p *pixeldrain) authHeader() string {
	if p.apiKey == "" {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+p.apiKey))
}

// APIError, pixeldrain'in hata zarfıdır. Value alanı kararların dayanağıdır;
// message insan içindir ve değişebilir.
//
// Bilinen value kodları (hepsi 403, aksi belirtilmedikçe):
// file_rate_limited_captcha_required, virus_detected_captcha_required,
// hotlink_detected, ip_download_limited_captcha_required,
// max_concurrent_downloads, transfer_limit_exceeded, download_limit_exceeded,
// unavailable_for_legal_reasons (451), not_found (404), recpatcha_failed (424).
type APIError struct {
	Status  int
	Value   string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("pixeldrain %d %s: %s", e.Status, e.Value, e.Message)
}

// Retryable, hatanın bekleyip tekrar denemeye değer olup olmadığını söyler.
// Adım 6'daki backoff katmanı bunu okur. Captcha isteyen kodlar retryable
// DEĞİLDİR: beklemek çözmez, kullanıcıya söylemek gerekir.
func (e *APIError) Retryable() bool {
	switch e.Value {
	case "transfer_limit_exceeded", "download_limit_exceeded", "max_concurrent_downloads":
		return true
	default:
		return false
	}
}

// CaptchaRequired, aracın durup kullanıcıya haber vermesi gereken durumlar.
// Kapsam sınırı: captcha çözmeye çalışmıyoruz.
func (e *APIError) CaptchaRequired() bool {
	return strings.HasSuffix(e.Value, "_captcha_required") || e.Value == "recpatcha_failed"
}

type refKind int

const (
	refAlbum refKind = iota
	refFile
)

type ref struct {
	kind refKind
	id   string
	host string // isteklerin gideceği host; girdi URL'inin kendi host'u
}

// Match, URL'in bu resolver'a ait olup olmadığını söyler.
// LegacyDomains da kabul edilir: ölü bir domain'den gelen linki TANIMAK gerekir,
// o domain'e istek atmak gerekmez.
func (p *pixeldrain) Match(u string) bool {
	_, err := p.parse(u)
	return err == nil
}

func (p *pixeldrain) parse(raw string) (ref, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ref{}, errors.New("boş URL")
	}
	if !strings.Contains(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ref{}, fmt.Errorf("URL ayrıştırılamadı: %w", err)
	}
	host := normalizeHost(u.Host)
	known := MatchHost(host, p.cfg.Domains) || MatchHost(host, p.cfg.LegacyDomains)
	if !known {
		return ref{}, fmt.Errorf("bilinmeyen host: %s", host)
	}

	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(seg) == 0 || seg[0] == "" {
		return ref{}, errors.New("yol boş")
	}

	switch seg[0] {
	case "l":
		if len(seg) < 2 || seg[1] == "" {
			return ref{}, errors.New("albüm id yok")
		}
		return ref{kind: refAlbum, id: seg[1], host: host}, nil
	case "u":
		if len(seg) < 2 || seg[1] == "" {
			return ref{}, errors.New("dosya id yok")
		}
		return ref{kind: refFile, id: seg[1], host: host}, nil
	case "api":
		// /api/file/{id} ve /api/file/{id}/info
		if len(seg) >= 3 && seg[1] == "file" && seg[2] != "" {
			return ref{kind: refFile, id: seg[2], host: host}, nil
		}
		if len(seg) >= 3 && seg[1] == "list" && seg[2] != "" {
			return ref{kind: refAlbum, id: seg[2], host: host}, nil
		}
		return ref{}, errors.New("desteklenmeyen api yolu")
	default:
		// pixeldra.in/{id} biçimindeki kısa link. Sadece tek segmentte geçerli.
		if len(seg) == 1 {
			return ref{kind: refFile, id: seg[0], host: host}, nil
		}
		return ref{}, fmt.Errorf("desteklenmeyen yol: /%s", strings.Join(seg, "/"))
	}
}

// fetchHost, isteklerin gideceği host'u seçer. Girdi URL'i ölü bir domain'den
// geliyorsa (LegacyDomains) o host'a istek atılmaz; aktif listenin ilki kullanılır.
func (p *pixeldrain) fetchHost(r ref) (string, error) {
	if MatchHost(r.host, p.cfg.Domains) {
		return r.host, nil
	}
	if len(p.cfg.Domains) == 0 {
		return "", errors.New("aktif domain listesi boş")
	}
	first := p.cfg.Domains[0]
	if strings.ContainsRune(first, '*') {
		return "", fmt.Errorf("aktif domain listesinin ilki joker: %q", first)
	}
	return normalizeHost(first), nil
}

func (p *pixeldrain) client() *http.Client {
	if p.cfg.HTTPClient != nil {
		return p.cfg.HTTPClient
	}
	return http.DefaultClient
}

// get, bir API çağrısı yapar. pageURL, politikanın "item_page" olduğu durumda
// Referer olarak kullanılacak İNSAN sayfasıdır; istenen API adresi değil.
// İkisini karıştırmak Referer'ı anlamsız kılar (ve bunkr'da doğrudan kırar).
func (p *pixeldrain) get(ctx context.Context, rawURL, pageURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Errorf(LayerFetch, rawURL, "istek kurulamadı: %v", err)
	}
	if p.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", p.cfg.UserAgent)
	}
	if a := p.authHeader(); a != "" {
		req.Header.Set("Authorization", a)
	}
	// RefererPolicy pixeldrain'de "none" olmalı: yanlış Referer tam olarak
	// hotlink_detected tetikler. Politika açıkça origin/item_page ise uygulanır.
	switch p.cfg.RefererPolicy {
	case RefererOrigin:
		req.Header.Set("Referer", "https://"+req.URL.Host+"/")
	case RefererItemPage:
		if pageURL != "" {
			req.Header.Set("Referer", pageURL)
		}
	}

	resp, err := p.client().Do(req)
	if err != nil {
		return classifyTransportError(rawURL, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return Errorf(LayerFetch, rawURL, "gövde okunamadı: %v", readErr)
	}

	if resp.StatusCode != http.StatusOK {
		apiErr := &APIError{Status: resp.StatusCode}
		var env struct {
			Value   string `json:"value"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &env) == nil && env.Value != "" {
			apiErr.Value, apiErr.Message = env.Value, env.Message
		} else {
			apiErr.Value = "http_" + resp.Status
			apiErr.Message = strings.TrimSpace(string(body[:min(len(body), 200)]))
		}
		layer := LayerFetch
		if resp.StatusCode == http.StatusForbidden && looksLikeChallenge(resp, body) {
			layer = LayerChallenge
		}
		return &LayerError{Layer: layer, Err: apiErr, Evidence: rawURL}
	}

	if err := json.Unmarshal(body, out); err != nil {
		return Errorf(LayerParse, rawURL, "JSON çözülemedi: %v", err)
	}
	return nil
}

// getRaw, get ile aynı yolu izler ama çözülmüş yapı yerine ham gövdeyi
// döndürür. doctor'ın --record'u için gerekli: kaydedilecek şey çözülmüş yapı
// değil, sunucunun gerçekten gönderdiği baytlar.
func (p *pixeldrain) getRaw(ctx context.Context, rawURL string) ([]byte, error) {
	var raw json.RawMessage
	if err := p.get(ctx, rawURL, "", &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// classifyTransportError, taşıma katmanı hatasını doğru Layer'a bağlar.
// DNS ile TLS ayrımı kritik: ikisi de "site açılmıyor" gibi görünür ama
// düzeltmeleri farklıdır.
func classifyTransportError(rawURL string, err error) error {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &LayerError{Layer: LayerDNS, Err: err, Evidence: rawURL}
	}
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) {
		return &LayerError{
			Layer: LayerTLS, Err: err,
			Evidence: rawURL + " (sertifika doğrulanamadı; operatör araya girmiş olabilir)",
		}
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return &LayerError{Layer: LayerTLS, Err: err, Evidence: rawURL}
	}
	return &LayerError{Layer: LayerFetch, Err: err, Evidence: rawURL}
}

// ClassifyStatus, indiricinin 403/410 yanıtlarını doğru yorumlamasını sağlar.
// site.StatusClassifier arayüzünü karşılar.
//
// Bu olmadan her 403 "imzalı URL süresi doldu" sayılır ve araç rate limitliyken
// URL'i yeniden çözüp tekrar dener. pixeldrain ise transfer_limit_exceeded,
// hotlink_detected ve *_captcha_required durumlarını da 403 ile bildiriyor.
func (p *pixeldrain) ClassifyStatus(resp *http.Response, body []byte) error {
	var env struct {
		Value   string `json:"value"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &env) != nil || env.Value == "" {
		// Tanınabilir bir API zarfı değil: CDN'in imzalı URL reddi olabilir.
		// nil dönmek "varsayılanı uygula" demek.
		return nil
	}
	evidence := ""
	if resp.Request != nil && resp.Request.URL != nil {
		evidence = resp.Request.URL.String()
	}
	layer := LayerFetch
	if resp.StatusCode == http.StatusForbidden && looksLikeChallenge(resp, body) {
		layer = LayerChallenge
	}
	return &LayerError{
		Layer:    layer,
		Err:      &APIError{Status: resp.StatusCode, Value: env.Value, Message: env.Message},
		Evidence: evidence,
	}
}

// looksLikeChallenge, 403'ün Cloudflare challenge olup olmadığını söyler.
func looksLikeChallenge(resp *http.Response, body []byte) bool {
	if resp.Header.Get("CF-Mitigated") != "" {
		return true
	}
	low := strings.ToLower(string(body[:min(len(body), 4096)]))
	for _, m := range []string{"just a moment", "cf_chl", "challenge-platform", "enable javascript and cookies"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

type fileInfo struct {
	Success    bool   `json:"success"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	MimeType   string `json:"mime_type"`
	HashSHA256 string `json:"hash_sha256"`
}

type listInfo struct {
	Success   bool       `json:"success"`
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	FileCount int        `json:"file_count"`
	Files     []fileInfo `json:"files"`
}

func (p *pixeldrain) apiBase(host string) string { return "https://" + host + "/api" }

// downloadURL, dosyanın gerçek indirme adresidir. pixeldrain'de bu adres
// süreli/imzalı DEĞİL, o yüzden ResolveOne yalnızca bütünlük içindir.
func (p *pixeldrain) downloadURL(host, id string) string {
	return p.apiBase(host) + "/file/" + url.PathEscape(id) + "?download"
}

func (p *pixeldrain) itemPage(host, id string) string {
	return "https://" + host + "/u/" + url.PathEscape(id)
}

func (p *pixeldrain) albumPage(host, id string) string {
	return "https://" + host + "/l/" + url.PathEscape(id)
}

// itemHeaders, indirme isteğine eklenecek başlıkları politikadan türetir.
//
// Bu olmadan referer_policy yalnızca API çağrılarını etkiler ve asıl transfer
// isteğine hiç yansımaz. pixeldrain'de politika "none" olduğu için sonuç boş;
// ama bunkr item sayfası Referer'ını transferde ZORUNLU kıldığı için mekanizma
// şimdiden doğru yerde olmak zorunda.
func (p *pixeldrain) itemHeaders(itemPage string) map[string]string {
	h := map[string]string{}
	switch p.cfg.RefererPolicy {
	case RefererItemPage:
		if itemPage != "" {
			h["Referer"] = itemPage
		}
	case RefererOrigin:
		if u, err := url.Parse(itemPage); err == nil && u.Host != "" {
			h["Referer"] = u.Scheme + "://" + u.Host + "/"
		}
	}
	// Ücretli hesabın anahtarı transfer isteğine de gidiyor: hotlink ve
	// captcha sınırı tam olarak bu istekte biniyor, API çağrısında değil.
	if a := p.authHeader(); a != "" {
		h["Authorization"] = a
	}
	if len(h) == 0 {
		return nil
	}
	return h
}

// Resolve, albüm için TEK istek atar ve gömülü files[] dizisinden Item üretir.
//
// Her dosya için ayrı /info çağrılmaz: 200 dosyalık albümde 201 istek eder ve
// Premise 3'teki rate limit'i kendi elinle tetiklersin.
//
// Bedeli yok: pixeldrain'in resmi API belgesi files[] şemasını eksik listeliyor
// ama gerçek yanıt dolu hash_sha256 taşıyor (34 dosyalık bir albümde 34/34
// doğrulandı). Yani albüm üyelerinde de sha256 doğrulaması çalışıyor ve toplu
// /info çağrısına hiç gerek yok.
func (p *pixeldrain) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	r, err := p.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}
	host, err := p.fetchHost(r)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}

	if r.kind == refFile {
		item, err := p.resolveFile(ctx, host, r.id)
		if err != nil {
			return nil, err
		}
		if err := yield(item); err != nil {
			return nil, err
		}
		return nil, nil
	}

	var list listInfo
	if err := p.get(ctx, p.apiBase(host)+"/list/"+url.PathEscape(r.id),
		p.albumPage(host, r.id), &list); err != nil {
		return nil, err
	}
	if !list.Success {
		return nil, Errorf(LayerParse, u, "liste success=false döndü")
	}

	dir := sanitizeDirLabel(list.Title, list.ID)
	var itemErrs []ItemError

	// file_count ile files[] uzunlugu ayrisiyorsa liste eksik geldi. Sessizce
	// daha az dosya indirip cikis 0 vermek, "sessiz basarisizlik yok" ilkesinin
	// dogrudan ihlali; albumu dusurmeden hata olarak bildiriyoruz.
	if list.FileCount > 0 && list.FileCount != len(list.Files) {
		itemErrs = append(itemErrs, ItemError{
			URL: u,
			Err: Errorf(LayerParse, fmt.Sprintf("file_count=%d, files[]=%d",
				list.FileCount, len(list.Files)), "liste eksik geldi"),
		})
	}
	for i, f := range list.Files {
		if f.ID == "" {
			itemErrs = append(itemErrs, ItemError{
				URL: u,
				Err: Errorf(LayerParse, fmt.Sprintf("files[%d]", i), "id boş"),
			})
			continue
		}
		sourcePage := p.itemPage(host, f.ID)
		item := Item{
			URL:        p.downloadURL(host, f.ID),
			SourcePage: sourcePage,
			Headers:    p.itemHeaders(sourcePage),
			Dir:        dir,
			Filename:   f.Name,
			SHA256:     f.HashSHA256, // liste yanıtında dolu geliyor; indirici doğrular
			Size:       f.Size,
			Index:      i,
		}
		if item.Size == 0 {
			item.Size = -1
		}
		if err := yield(item); err != nil {
			return itemErrs, err
		}
	}
	return itemErrs, nil
}

func (p *pixeldrain) resolveFile(ctx context.Context, host, id string) (Item, error) {
	var info fileInfo
	if err := p.get(ctx, p.apiBase(host)+"/file/"+url.PathEscape(id)+"/info",
		p.itemPage(host, id), &info); err != nil {
		return Item{}, err
	}
	if !info.Success {
		return Item{}, Errorf(LayerParse, id, "info success=false döndü")
	}
	size := info.Size
	if size == 0 {
		size = -1
	}
	sourcePage := p.itemPage(host, info.ID)
	return Item{
		URL:        p.downloadURL(host, info.ID),
		SourcePage: sourcePage,
		Headers:    p.itemHeaders(sourcePage),
		Dir:        "",
		Filename:   info.Name,
		SHA256:     info.HashSHA256,
		Size:       size,
		Index:      0,
	}, nil
}

// ResolveOne, item sayfasından tek bir Item'ı yeniden çözer.
// pixeldrain'de indirme adresi imzalı olmadığı için pratikte gerekmez;
// sözleşmenin bir parçası olduğu ve bunkr'da hayati olduğu için burada da doğru
// çalışır.
func (p *pixeldrain) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	r, err := p.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if r.kind != refFile {
		return Item{}, Errorf(LayerParse, sourcePage, "item sayfası bekleniyordu, albüm geldi")
	}
	host, err := p.fetchHost(r)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	return p.resolveFile(ctx, host, r.id)
}

// Diagnose, canary listesini sırayla dener ve ilk çalışanda durur.
// Tek canary'ye bağlanmıyor: ölçüm, bir domain'in operatör tarafından
// engellenebildiğini ve sitenin yine de çalışır durumda olabildiğini gösterdi.
func (p *pixeldrain) Diagnose(ctx context.Context) ([]LayerResult, error) {
	if len(p.cfg.CanaryURLs) == 0 {
		return nil, errors.New("canary URL listesi boş")
	}

	var last []LayerResult
	for _, canary := range p.cfg.CanaryURLs {
		res := p.diagnoseOne(ctx, canary)
		last = res
		if !hasFail(res) {
			return res, nil
		}
	}
	return last, nil
}

func (p *pixeldrain) diagnoseOne(ctx context.Context, canary string) []LayerResult {
	out := make([]LayerResult, 0, 5)

	u, err := url.Parse(canary)
	if err != nil {
		return append(out, LayerResult{
			Layer: LayerDNS, Status: StatusFail,
			Detail: "canary URL ayrıştırılamadı", Evidence: canary,
		})
	}
	host := normalizeHost(u.Host)

	addrs, dnsErr := net.DefaultResolver.LookupIPAddr(ctx, host)
	if dnsErr != nil {
		return append(out, LayerResult{
			Layer: LayerDNS, Status: StatusFail,
			Detail: "çözümlenemedi", Evidence: host + ": " + dnsErr.Error(),
		})
	}
	ips := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP.String())
	}
	out = append(out, LayerResult{
		Layer: LayerDNS, Status: StatusOK,
		Detail: fmt.Sprintf("%d adres", len(ips)), Evidence: strings.Join(ips, ", "),
	})

	var probe struct {
		Success bool `json:"success"`
	}
	raw, rawErr := p.getRaw(ctx, canary)
	p.cfg.Recordln("canary.json", raw)
	err = rawErr
	if err == nil {
		if jerr := json.Unmarshal(raw, &probe); jerr != nil {
			err = Errorf(LayerParse, canary, "JSON çözülemedi: %v", jerr)
		}
	}

	layer, _ := LayerOf(err)
	switch {
	case err == nil:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "el sıkışma tamam"},
			LayerResult{Layer: LayerChallenge, Status: StatusOK, Detail: "challenge yok"},
			LayerResult{Layer: LayerFetch, Status: StatusOK,
				Detail: fmt.Sprintf("200, %d bayt", len(raw))},
			LayerResult{Layer: LayerParse, Status: StatusOK, Detail: "JSON çözüldü"},
			// pixeldrain'de ayrı bir item sayfası yok: liste yanıtı dosya
			// id'lerini doğrudan taşıyor, zincir tek adım.
			LayerResult{Layer: LayerItemPage, Status: StatusOK,
				Detail: "pixeldrain'de ayrı item sayfası yok, zincir tek adım"},
			// Ayrı bir CDN de yok: dosyalar sitenin kendi domaininden geliyor,
			// yani izlenecek dönen bir host kümesi yok. Bu bir boşluk değil,
			// sitenin mimarisi.
			LayerResult{Layer: LayerCDN, Status: StatusOK,
				Detail: "dosyalar site domaininden servis ediliyor, ayrı CDN yok"},
		)
	case layer == LayerTLS:
		out = append(out, LayerResult{
			Layer: LayerTLS, Status: StatusFail,
			Detail: "sertifika/el sıkışma başarısız", Evidence: err.Error(),
		})
	case layer == LayerChallenge:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "el sıkışma tamam"},
			LayerResult{Layer: LayerChallenge, Status: StatusFail,
				Detail: "Cloudflare challenge", Evidence: err.Error()},
		)
	case layer == LayerParse:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "el sıkışma tamam"},
			LayerResult{Layer: LayerChallenge, Status: StatusOK, Detail: "challenge yok"},
			LayerResult{Layer: LayerFetch, Status: StatusOK, Detail: "200"},
			LayerResult{Layer: LayerParse, Status: StatusFail,
				Detail: "yanıt JSON değil", Evidence: err.Error()},
		)
	default:
		out = append(out, LayerResult{
			Layer: layer, Status: StatusFail,
			Detail: "istek başarısız", Evidence: err.Error(),
		})
	}
	return out
}

func hasFail(rs []LayerResult) bool {
	for _, r := range rs {
		if r.Status == StatusFail {
			return true
		}
	}
	return false
}

// sanitizeDirLabel, albüm klasörü için ham bir etiket üretir.
// Tam Windows temizliği adım 5'te internal/dl/names.go'da yapılır; burada
// yalnızca yol ayırıcıları etkisizleştirilir ki Dir tek bir bileşen kalsın.
func sanitizeDirLabel(title, id string) string {
	t := strings.TrimSpace(title)
	t = strings.NewReplacer("/", "-", "\\", "-", "\x00", "").Replace(t)
	t = strings.TrimSpace(t)
	if t == "" {
		return id
	}
	return t
}
