package site

import (
	"bytes"
	"context"
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
	"sync/atomic"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/megacrypto"
)

// MegaName, registry kayıt anahtarı.
const MegaName = "mega"

// mega'nın diğer iki siteden temel farkı: dosyalar istemci tarafında şifreli.
// Adres yeterli değil; anahtar linkteki #'ten sonra duruyor, sunucuya hiç
// gitmiyor ve içerik indirilirken çözülmek zorunda. Bu yüzden bu resolver
// StreamDecoder'ı da uyguluyor. Kripto tarafı internal/megacrypto'da.
//
// API tek uç: https://g.api.mega.co.nz/cs. Komutlar JSON dizisi olarak POST
// edilir, yanıt da dizi gelir; hata durumunda eleman (veya tüm gövde) negatif
// bir tamsayıdır.

const (
	ExtraMegaAPI   = "api_endpoint"
	defaultMegaAPI = "https://g.api.mega.co.nz/cs"

	// megaBatch, tek API çağrısında istenecek indirme adresi sayısı. mega
	// toplu komutu destekliyor; 200 dosyalık bir klasörü 200 ayrı istekle
	// çözmek yerine 4 istekle çözüyoruz.
	megaBatch = 50
)

// NewMega, registry'ye verilecek fabrikadır.
func NewMega(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	return &mega{
		cfg:      cfg,
		api:      cfg.ExtraOr(ExtraMegaAPI, defaultMegaAPI),
		nodeKeys: map[string][]byte{},
	}
}

type mega struct {
	cfg SiteConfig
	api string
	seq atomic.Int64

	// nodeKeys, klasör linklerindeki düğümlerin paketli anahtarları
	// (düğüm -> 32 bayt). ResolveOne bir klasör dosyasını tek başına
	// yenilemek zorunda kaldığında klasörü baştan listelememek için.
	mu       sync.Mutex
	nodeKeys map[string][]byte
}

// ---------- Link tanıma ----------

type megaKind int

const (
	megaFile megaKind = iota
	megaFolder
)

type megaRef struct {
	kind   megaKind
	handle string // dosya veya klasör tanıtıcısı
	key    []byte // çözülmüş: dosya 32 bayt, klasör 16 bayt
	node   string // klasör içinde seçili düğüm (opsiyonel)
}

func (m *mega) Match(u string) bool {
	_, err := m.parse(u)
	return err == nil
}

// parse, mega'nın dört link biçimini tanır:
//
//	https://mega.nz/file/<h>#<k>
//	https://mega.nz/folder/<h>#<k>[/file/<n>|/folder/<n>]
//	https://mega.nz/#!<h>!<k>                  (eski)
//	https://mega.nz/#F!<h>!<k>[!<n>|?<n>]       (eski klasör)
//
// Anahtarın uzunluğu türü doğrular: dosya 32, klasör 16 bayt. Yanlış
// uzunlukta bir anahtar linkin kırpılarak kopyalandığı anlamına gelir ve bunu
// burada söylemek, indirme sonunda "meta-MAC uyuşmuyor" demekten iyidir.
func (m *mega) parse(raw string) (megaRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return megaRef{}, errors.New("boş URL")
	}
	if !strings.Contains(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return megaRef{}, fmt.Errorf("URL ayrıştırılamadı: %w", err)
	}
	host := strings.TrimPrefix(normalizeHost(u.Host), "www.")
	known := MatchHost(host, m.cfg.Domains) ||
		MatchHost(host, m.cfg.LegacyDomains) ||
		MatchHost(host, m.cfg.MatchPatterns)
	if !known {
		return megaRef{}, fmt.Errorf("bilinmeyen host: %s", host)
	}

	var ref megaRef
	frag := u.Fragment
	seg := strings.Split(strings.Trim(u.Path, "/"), "/")

	switch {
	case len(seg) >= 2 && (seg[0] == "file" || seg[0] == "folder"):
		ref.handle = seg[1]
		parts := strings.Split(frag, "/")
		keyStr := parts[0]
		if seg[0] == "file" {
			ref.kind = megaFile
		} else {
			ref.kind = megaFolder
			// #<k>/file/<n> veya #<k>/folder/<n>
			if len(parts) >= 3 && (parts[1] == "file" || parts[1] == "folder") {
				ref.node = parts[2]
			}
		}
		ref.key, err = megacrypto.B64Decode(keyStr)
	case strings.HasPrefix(frag, "F!"):
		ref.kind = megaFolder
		rest := strings.TrimPrefix(frag, "F!")
		// !<n> veya ?<n> ile seçili düğüm
		if i := strings.IndexAny(rest, "!?"); i >= 0 {
			ref.handle = rest[:i]
			rest = rest[i+1:]
			if j := strings.IndexAny(rest, "!?"); j >= 0 {
				ref.key, err = megacrypto.B64Decode(rest[:j])
				ref.node = rest[j+1:]
			} else {
				ref.key, err = megacrypto.B64Decode(rest)
			}
		}
	case strings.HasPrefix(frag, "!"):
		ref.kind = megaFile
		rest := strings.TrimPrefix(frag, "!")
		if i := strings.Index(rest, "!"); i >= 0 {
			ref.handle = rest[:i]
			ref.key, err = megacrypto.B64Decode(rest[i+1:])
		}
	default:
		return megaRef{}, errors.New("mega linki değil: /file/, /folder/ veya #! bekleniyordu")
	}

	if err != nil {
		return megaRef{}, fmt.Errorf("anahtar base64 değil: %w", err)
	}
	if ref.handle == "" {
		return megaRef{}, errors.New("tanıtıcı eksik")
	}
	want := 32
	if ref.kind == megaFolder {
		want = 16
	}
	if len(ref.key) != want {
		return megaRef{}, fmt.Errorf("anahtar %d bayt, %d bekleniyordu: link kırpılmış olabilir", len(ref.key), want)
	}
	return ref, nil
}

// canonical, ResolveOne'ın yeniden ayrıştırabileceği tek biçimli adres üretir.
func (ref megaRef) canonical(node string) string {
	if ref.kind == megaFile {
		return "https://mega.nz/file/" + ref.handle + "#" + megacrypto.B64Encode(ref.key)
	}
	s := "https://mega.nz/folder/" + ref.handle + "#" + megacrypto.B64Encode(ref.key)
	if node != "" {
		s += "/file/" + node
	}
	return s
}

// ---------- API ----------

func (m *mega) client() *http.Client {
	if m.cfg.HTTPClient != nil {
		return m.cfg.HTTPClient
	}
	return http.DefaultClient
}

// megaAPIError, API'nin negatif tamsayı hatalarını katmana çevirir.
//
// Kota (-17) ve engel (-16) diğerlerinden AYRI okunmalı: kota "birkaç saat
// bekle" demek, engel "bu dosya hiç gelmeyecek" demek. İkisini de "API hatası"
// diye geçmek kullanıcıyı config kurcalamaya gönderir.
func megaAPIError(code int, evidence string) error {
	switch code {
	case -3, -4, -18:
		return &megaTransient{code: code}
	case -2:
		return Errorf(LayerParse, evidence, "API isteği reddedildi (%d): tanıtıcı geçersiz olabilir", code)
	case -9:
		return Errorf(LayerItemPage, evidence, "dosya yok veya kaldırılmış (%d)", code)
	case -11:
		return Errorf(LayerItemPage, evidence, "erişim reddedildi (%d)", code)
	case -14:
		return Errorf(LayerItemPage, evidence, "anahtar geçersiz (%d): link kırpılmış olabilir", code)
	case -16:
		return Errorf(LayerItemPage, evidence, "dosya engellenmiş (%d): telif veya kötüye kullanım bildirimi", code)
	case -17:
		return Errorf(LayerCDN, evidence, "mega aktarım kotası doldu (%d): IP başına sınır, birkaç saat sonra tekrar deneyin", code)
	default:
		return Errorf(LayerFetch, evidence, "API hatası %d", code)
	}
}

// megaTransient, API'nin "birazdan tekrar dene" dediği durumlar.
type megaTransient struct{ code int }

func (e *megaTransient) Error() string { return fmt.Sprintf("mega API geçici hata %d", e.code) }

// call, komutları tek istekte gönderir ve komut başına ham sonuçları döndürür.
//
// Geçici hatalarda (-3 EAGAIN, -4 rate limit, -18, 5xx) kendi içinde birkaç
// kez yeniden deniyor: resolver'ların çağrısı indirme politikasının dışında
// kalıyor, yani burada denemezsek hiç denenmez.
func (m *mega) call(ctx context.Context, folder string, cmds []any) ([]json.RawMessage, error) {
	payload, err := json.Marshal(cmds)
	if err != nil {
		return nil, err
	}

	const attempts = 4
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			d := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(d):
			}
		}
		out, err := m.callOnce(ctx, folder, payload)
		if err == nil {
			return out, nil
		}
		last = err
		var tr *megaTransient
		if !errors.As(err, &tr) {
			return nil, err
		}
		m.cfg.Logln("mega: %v, tekrar deneniyor (%d/%d)", err, attempt+1, attempts)
	}
	return nil, Errorf(LayerFetch, m.api, "API art arda geçici hata verdi: %v", last)
}

func (m *mega) callOnce(ctx context.Context, folder string, payload []byte) ([]json.RawMessage, error) {
	endpoint := m.api + "?id=" + strconv.FormatInt(m.seq.Add(1), 10)
	if folder != "" {
		endpoint += "&n=" + url.QueryEscape(folder)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, Errorf(LayerFetch, endpoint, "istek kurulamadı: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if m.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", m.cfg.UserAgent)
	}

	resp, err := m.client().Do(req)
	if err != nil {
		return nil, &LayerError{Layer: LayerFetch, Err: unwrapURLError(err), Evidence: endpoint}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	m.cfg.Recordln("api.json", body)

	if resp.StatusCode >= 500 {
		return nil, &megaTransient{code: -resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, Errorf(LayerFetch, endpoint, "API HTTP %s", resp.Status)
	}

	trim := bytes.TrimSpace(body)
	// Tüm gövde tek bir sayı olabilir: [{"a":"g"}] -> -3
	if len(trim) > 0 && trim[0] != '[' {
		code, cerr := strconv.Atoi(string(trim))
		if cerr != nil {
			return nil, Errorf(LayerParse, endpoint, "API yanıtı ne dizi ne sayı: %.60s", trim)
		}
		return nil, megaAPIError(code, endpoint)
	}
	var results []json.RawMessage
	if err := json.Unmarshal(trim, &results); err != nil {
		return nil, Errorf(LayerParse, endpoint, "API yanıtı JSON değil: %v", err)
	}
	return results, nil
}

// decodeResult, tek bir komut sonucunu out'a açar; sayıysa hataya çevirir.
func (m *mega) decodeResult(raw json.RawMessage, out any, evidence string) error {
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && (t[0] == '-' || (t[0] >= '0' && t[0] <= '9')) {
		code, err := strconv.Atoi(string(t))
		if err != nil {
			return Errorf(LayerParse, evidence, "API sonucu anlaşılamadı: %.60s", t)
		}
		return megaAPIError(code, evidence)
	}
	if err := json.Unmarshal(t, out); err != nil {
		return Errorf(LayerParse, evidence, "API sonucu JSON değil: %v", err)
	}
	return nil
}

// megaGetResp, "g" komutunun yanıtı: boyut, şifreli öznitelik, indirme adresi.
type megaGetResp struct {
	Size  int64  `json:"s"`
	Attrs string `json:"at"`
	URL   string `json:"g"`
	Err   int    `json:"e"`
}

// megaNode, klasör listesindeki bir düğüm.
type megaNode struct {
	Handle string `json:"h"`
	Parent string `json:"p"`
	Type   int    `json:"t"` // 0 dosya, 1 klasör
	Attrs  string `json:"a"`
	Key    string `json:"k"` // "<paylaşım>:<base64 şifreli anahtar>"
	Size   int64  `json:"s"`
}

// ---------- Resolver ----------

func (m *mega) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	ref, err := m.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}
	if ref.kind == megaFile {
		it, err := m.resolveFile(ctx, ref)
		if err != nil {
			return nil, err
		}
		return nil, yield(it)
	}
	return m.resolveFolder(ctx, ref, yield)
}

// resolveFile, tek dosya linkini çözer.
func (m *mega) resolveFile(ctx context.Context, ref megaRef) (Item, error) {
	src := ref.canonical("")
	results, err := m.call(ctx, "", []any{map[string]any{"a": "g", "g": 1, "p": ref.handle}})
	if err != nil {
		return Item{}, err
	}
	if len(results) == 0 {
		return Item{}, Errorf(LayerParse, src, "API boş dizi döndü")
	}
	var g megaGetResp
	if err := m.decodeResult(results[0], &g, src); err != nil {
		return Item{}, err
	}
	return m.itemFromGet(g, ref.key, src, "", 0)
}

// itemFromGet, "g" yanıtı ve paketli anahtardan Item kurar.
func (m *mega) itemFromGet(g megaGetResp, packed []byte, src, dir string, index int) (Item, error) {
	if g.Err != 0 {
		return Item{}, megaAPIError(g.Err, src)
	}
	if g.URL == "" {
		return Item{}, Errorf(LayerItemPage, src, "API indirme adresi vermedi")
	}
	key, err := megacrypto.UnpackFileKey(packed)
	if err != nil {
		return Item{}, Errorf(LayerParse, src, "%v", err)
	}
	attrs, err := megacrypto.DecryptAttrs(key.AES, g.Attrs)
	if err != nil {
		return Item{}, Errorf(LayerParse, src, "%v", err)
	}
	if attrs.Name == "" {
		return Item{}, Errorf(LayerParse, src, "öznitelikte dosya adı yok")
	}
	return Item{
		URL:        g.URL,
		SourcePage: src,
		Dir:        dir,
		Filename:   attrs.Name,
		Size:       g.Size,
		Index:      index,
		Secret:     packed,
	}, nil
}

// megaEntry, klasör listesinden çıkan, indirilecek tek dosya.
type megaEntry struct {
	node   string
	packed []byte
	name   string
	size   int64
	dir    string
}

// listFolder, klasör linkinin düğümlerini çözer ve indirilecek dosyaları
// klasör yolu ile birlikte döndürür. ref.node doluysa yalnızca o düğüm (dosya)
// veya o alt klasörün altındakiler.
func (m *mega) listFolder(ctx context.Context, ref megaRef) ([]megaEntry, error) {
	src := ref.canonical("")
	results, err := m.call(ctx, ref.handle, []any{map[string]any{"a": "f", "c": 1, "r": 1}})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, Errorf(LayerParse, src, "API boş dizi döndü")
	}
	var tree struct {
		Nodes []megaNode `json:"f"`
	}
	if err := m.decodeResult(results[0], &tree, src); err != nil {
		return nil, err
	}
	if len(tree.Nodes) == 0 {
		return nil, Errorf(LayerParse, src, "klasör boş veya listelenemedi")
	}

	// Klasör adları: her klasör düğümünün özniteliği klasör anahtarıyla
	// (dosyalarda olduğu gibi türetilmiş anahtarla DEĞİL, doğrudan 16 baytla)
	// çözülüyor. Alt klasörlerin kendi anahtarı da "k" alanında geliyor.
	byHandle := map[string]megaNode{}
	folderName := map[string]string{}
	for _, n := range tree.Nodes {
		byHandle[n.Handle] = n
		if n.Type == 1 {
			fk := ref.key
			if n.Handle != ref.handle {
				if dk, derr := m.nodeKey(ref, n); derr == nil && len(dk) == 16 {
					fk = dk
				}
			}
			if a, aerr := megacrypto.DecryptAttrs(fk, n.Attrs); aerr == nil && a.Name != "" {
				folderName[n.Handle] = a.Name
			} else {
				folderName[n.Handle] = n.Handle
			}
		}
	}

	// Alt klasör yolu "Kök/Alt" olarak kuruluyor; indirici "/" karakterini
	// "-" yapıp tek klasöre indiriyor ("Kök-Alt"). Yapı bilgisi korunuyor,
	// iç içe klasör açılmıyor. v1 için yeterli.
	pathOf := func(h string) string {
		var parts []string
		for cur := h; cur != ""; {
			n, ok := byHandle[cur]
			if !ok {
				break
			}
			parts = append([]string{folderName[cur]}, parts...)
			if cur == ref.handle {
				break
			}
			cur = n.Parent
		}
		return strings.Join(parts, "/")
	}
	under := func(h string) bool {
		// h, ref.node'un altında mı (ref.node bir alt klasörse)?
		if ref.node == "" {
			return true
		}
		for cur := h; cur != ""; {
			if cur == ref.node {
				return true
			}
			n, ok := byHandle[cur]
			if !ok || cur == ref.handle {
				return false
			}
			cur = n.Parent
		}
		return false
	}

	var out []megaEntry
	for _, n := range tree.Nodes {
		if n.Type != 0 {
			continue
		}
		if ref.node != "" && n.Handle != ref.node && !under(n.Parent) {
			continue
		}
		packed, err := m.nodeKey(ref, n)
		if err != nil {
			m.cfg.Logln("mega: %s düğümünün anahtarı çözülemedi: %v", n.Handle, err)
			continue
		}
		key, err := megacrypto.UnpackFileKey(packed)
		if err != nil {
			m.cfg.Logln("mega: %s düğümü: %v", n.Handle, err)
			continue
		}
		attrs, err := megacrypto.DecryptAttrs(key.AES, n.Attrs)
		if err != nil || attrs.Name == "" {
			m.cfg.Logln("mega: %s düğümünün adı çözülemedi: %v", n.Handle, err)
			continue
		}
		out = append(out, megaEntry{
			node: n.Handle, packed: packed, name: attrs.Name, size: n.Size, dir: pathOf(n.Parent),
		})
	}

	m.mu.Lock()
	for _, e := range out {
		m.nodeKeys[e.node] = e.packed
	}
	m.mu.Unlock()
	return out, nil
}

// nodeKey, düğümün "k" alanındaki şifreli anahtarı klasör anahtarıyla açar.
// Alan "<paylaşım>:<anahtar>" biçiminde, birden fazlaysa "/" ile ayrılmış.
func (m *mega) nodeKey(ref megaRef, n megaNode) ([]byte, error) {
	if n.Key == "" {
		return nil, errors.New("k alanı boş")
	}
	chosen := ""
	for _, part := range strings.Split(n.Key, "/") {
		i := strings.Index(part, ":")
		if i < 0 {
			continue
		}
		if part[:i] == ref.handle || chosen == "" {
			chosen = part[i+1:]
			if part[:i] == ref.handle {
				break
			}
		}
	}
	if chosen == "" {
		return nil, errors.New("k alanında anahtar yok")
	}
	enc, err := megacrypto.B64Decode(chosen)
	if err != nil {
		return nil, err
	}
	return megacrypto.DecryptNodeKey(ref.key, enc)
}

// resolveFolder, klasördeki dosyaları toplu "g" çağrılarıyla çözer ve yield eder.
func (m *mega) resolveFolder(ctx context.Context, ref megaRef, yield func(Item) error) ([]ItemError, error) {
	entries, err := m.listFolder(ctx, ref)
	if err != nil {
		return nil, err
	}
	if ref.node != "" && len(entries) == 0 {
		return nil, Errorf(LayerItemPage, ref.canonical(ref.node), "seçili düğüm klasörde bulunamadı")
	}

	var itemErrs []ItemError
	index := 0
	for start := 0; start < len(entries); start += megaBatch {
		end := start + megaBatch
		if end > len(entries) {
			end = len(entries)
		}
		batch := entries[start:end]
		cmds := make([]any, 0, len(batch))
		for _, e := range batch {
			cmds = append(cmds, map[string]any{"a": "g", "g": 1, "n": e.node})
		}
		results, err := m.call(ctx, ref.handle, cmds)
		if err != nil {
			return itemErrs, err
		}
		if len(results) != len(batch) {
			return itemErrs, Errorf(LayerParse, ref.canonical(""),
				"API %d sonuç döndü, %d bekleniyordu", len(results), len(batch))
		}
		for i, e := range batch {
			src := ref.canonical(e.node)
			var g megaGetResp
			if derr := m.decodeResult(results[i], &g, src); derr != nil {
				itemErrs = append(itemErrs, ItemError{URL: src, Err: derr})
				continue
			}
			it, ierr := m.itemFromGet(g, e.packed, src, e.dir, index)
			if ierr != nil {
				itemErrs = append(itemErrs, ItemError{URL: src, Err: ierr})
				continue
			}
			// Listeden gelen ad ve boyut daha güvenilir: "g" bazen öznitelik
			// taşımıyor.
			if it.Filename == "" {
				it.Filename = e.name
			}
			if it.Size <= 0 {
				it.Size = e.size
			}
			index++
			if yerr := yield(it); yerr != nil {
				return itemErrs, yerr
			}
		}
	}
	return itemErrs, nil
}

// ResolveOne, süresi dolan indirme adresini tazeler.
func (m *mega) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	ref, err := m.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if ref.kind == megaFile {
		return m.resolveFile(ctx, ref)
	}
	if ref.node == "" {
		return Item{}, Errorf(LayerParse, sourcePage, "klasör linki tek item olarak çözülemez; /file/<düğüm> gerekli")
	}

	m.mu.Lock()
	packed, ok := m.nodeKeys[ref.node]
	m.mu.Unlock()
	if !ok {
		// Önbellekte yok (örn. süreç yeniden başladı): klasörü baştan listele.
		if _, lerr := m.listFolder(ctx, megaRef{kind: megaFolder, handle: ref.handle, key: ref.key}); lerr != nil {
			return Item{}, lerr
		}
		m.mu.Lock()
		packed, ok = m.nodeKeys[ref.node]
		m.mu.Unlock()
		if !ok {
			return Item{}, Errorf(LayerItemPage, sourcePage, "düğüm klasörde bulunamadı")
		}
	}

	results, err := m.call(ctx, ref.handle, []any{map[string]any{"a": "g", "g": 1, "n": ref.node}})
	if err != nil {
		return Item{}, err
	}
	if len(results) == 0 {
		return Item{}, Errorf(LayerParse, sourcePage, "API boş dizi döndü")
	}
	var g megaGetResp
	if err := m.decodeResult(results[0], &g, sourcePage); err != nil {
		return Item{}, err
	}
	return m.itemFromGet(g, packed, sourcePage, "", 0)
}

// ---------- İndirici kancaları ----------

// DecodeStream, site.StreamDecoder. Anahtar Item.Secret'ta taşınıyor.
func (m *mega) DecodeStream(it Item, offset int64, saved []byte, r io.Reader) (DecodedStream, error) {
	key, err := megacrypto.UnpackFileKey(it.Secret)
	if err != nil {
		return nil, fmt.Errorf("mega: item anahtarı yok veya bozuk: %w", err)
	}
	return megacrypto.NewStream(key, offset, saved, r)
}

// ClassifyStatus, site.StatusClassifier.
//
// 509, mega'nın IP başına aktarım kotası. Genel kurala göre "5xx, geçici"
// sayılıp tekrar tekrar denenirdi; oysa kota dolduysa saatlerce dolu kalır ve
// her deneme sayaçta yer tutar. Kalıcı hata olarak bildiriliyor.
//
// 403 ise nil dönüyor: indirici onu "adres süresi dolmuş" sayıp ResolveOne
// ile tazeliyor, mega'nın geçici adresleri için doğru tepki bu.
func (m *mega) ClassifyStatus(resp *http.Response, body []byte) error {
	if resp.StatusCode == 509 {
		evidence := ""
		if resp.Request != nil && resp.Request.URL != nil {
			evidence = resp.Request.URL.Host
		}
		return Errorf(LayerCDN, evidence,
			"mega aktarım kotası doldu (HTTP 509): IP başına sınır, birkaç saat sonra tekrar deneyin")
	}
	return nil
}

// ---------- Teşhis ----------

// Diagnose, API'ye kasıtlı olarak geçersiz bir komut gönderir. Beklenen yanıt
// negatif bir sayıdır: bu, "API'ye ulaştım, JSON konuşuyor ve bana cevap
// verdi" demektir. Gerçek bir dosya tanıtıcısına ihtiyaç yok.
func (m *mega) Diagnose(ctx context.Context) ([]LayerResult, error) {
	canaries := m.cfg.CanaryURLs
	if len(canaries) == 0 {
		canaries = []string{m.api}
	}
	var last []LayerResult
	for _, c := range canaries {
		res := m.diagnoseOne(ctx, c)
		last = res
		if !hasFail(res) {
			return res, nil
		}
	}
	return last, nil
}

func (m *mega) diagnoseOne(ctx context.Context, canary string) []LayerResult {
	var out []LayerResult
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

	probe := &mega{cfg: m.cfg, api: canary, nodeKeys: map[string][]byte{}}
	_, perr := probe.callOnce(ctx, "", []byte(`[{"a":"g","p":"AAAAAAAA"}]`))
	layer, _ := LayerOf(perr)
	switch {
	case perr == nil:
		// Geçersiz tanıtıcıya bile sonuç dizisi döndü; API konuşuyor.
		fallthrough
	case layer == LayerParse || layer == LayerItemPage:
		// Negatif sayı geldi (-2/-9): tam beklediğimiz şey.
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "el sıkışma tamam"},
			LayerResult{Layer: LayerChallenge, Status: StatusOK, Detail: "challenge yok"},
			LayerResult{Layer: LayerFetch, Status: StatusOK, Detail: "API yanıt verdi"},
			LayerResult{Layer: LayerParse, Status: StatusOK, Detail: "JSON çözüldü, hata kodu beklenen biçimde"},
			LayerResult{Layer: LayerItemPage, Status: StatusOK, Detail: "mega'da ayrı item sayfası yok, zincir API'den ibaret"},
			LayerResult{Layer: LayerCDN, Status: StatusOK, Detail: "indirme adresleri API'den geliyor; kota ancak indirirken görülür"},
		)
	case layer == LayerTLS:
		out = append(out, LayerResult{Layer: LayerTLS, Status: StatusFail,
			Detail: "el sıkışma başarısız", Evidence: collapseSpace(perr.Error())})
	default:
		out = append(out,
			LayerResult{Layer: LayerTLS, Status: StatusOK, Detail: "el sıkışma tamam"},
			LayerResult{Layer: LayerFetch, Status: StatusFail,
				Detail: "API'ye ulaşılamadı", Evidence: collapseSpace(perr.Error())})
	}
	return out
}
