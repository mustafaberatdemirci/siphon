// Package dl, devam edebilir indirmeden sorumludur.
//
// Tek doğruluk kaynağı `.part.state` dosyasıdır, `.part`'ın boyutu DEĞİL.
// Çökme anında ikisi ayrışır: işletim sistemi `.part`'a yazmış olabilir ama
// state güncellenmemiştir. Boyuta güvenirsen sha256 sessizce yanlış çıkar ve
// bu, aracın tüm amacı olan "sessiz başarısızlık yok" ilkesinin ihlalidir.
//
// Bunun simetriği de geçerli ve daha sinsi: state `.part`'tan ileri olabilir
// (dosya silinmiş veya kısalmış). O durumda resume, dosyanın başına offset
// kadar SIFIR deliği açar ve kaydedilmiş hash durumu sha256 kontrolünü
// GEÇİRİR. Bu yüzden state ile dosya her açılışta karşılaştırılır.
package dl

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// İndiricinin nihai adın yanına yazdığı sonekler. Sabit olmaları önemli:
// dosya adı sınırı bunlara yer ayırmak zorunda (bkz. Download).
const (
	partSuffix  = ".part"
	stateSuffix = ".part.state"
)

// Validator türleri. Hangisinin kullanıldığı state'te saklanır çünkü
// If-Range her ikisini de kabul eder ama anlamları farklıdır.
const (
	ValidatorNone         = ""
	ValidatorETag         = "etag"
	ValidatorLastModified = "last_modified"
)

// State, `.part.state` dosyasının şemasıdır.
type State struct {
	Offset        int64  `json:"offset"`
	Validator     string `json:"validator"`
	ValidatorType string `json:"validator_type"`
	SHA256State   []byte `json:"sha256_state"`

	// TotalSize YALNIZCA sunucunun Content-Length'idir; bilinmiyorsa -1.
	// resumable() buna bakar çünkü chunked yanıtta resume denenmez (tasarım
	// kararı). Resolver'ın bildirdiği boyut ayrı tutulur, yoksa chunked
	// indirmeler sessizce resume edilebilir hale gelirdi.
	TotalSize int64 `json:"total_size"`

	// ItemSize, resolver'ın bildirdiği boyuttur; bilinmiyorsa -1. Sadece
	// "tamamlandı mı" kontrolünde yedek olarak kullanılır, resume kararında
	// kullanılmaz.
	ItemSize int64 `json:"item_size"`
}

func freshState() State { return State{TotalSize: -1, ItemSize: -1} }

// resumable, bu state ile Range denemenin anlamlı olup olmadığını söyler.
// Validator yoksa resume DENENMEZ: sunucu dosyayı değiştirmişse iki farklı
// dosyanın parçalarını birbirine yapıştırırız ve bunu fark etmeyiz.
func (s State) resumable() bool {
	return s.Offset > 0 && s.Validator != "" && s.ValidatorType != ValidatorNone && s.TotalSize >= 0
}

// expectedTotal, tamamlanma kontrolü için kullanılacak boyuttur.
// Sunucunun değeri önce gelir; yoksa resolver'ın bildirdiği boyut.
func (s State) expectedTotal() int64 {
	if s.TotalSize >= 0 {
		return s.TotalSize
	}
	return s.ItemSize
}

// Reresolver, imzalı URL süresi dolduğunda (403/410) tek item'ı yeniden çözer.
type Reresolver func(ctx context.Context, sourcePage string) (site.Item, error)

// Classifier, bir hata durum kodunu siteye özgü biçimde sınıflandırır.
// nil dönerse dl kendi varsayılanını uygular.
//
// Bu kanca olmadan her 403 "imzalı URL süresi doldu" sayılır. pixeldrain ise
// rate limit, hotlink ve captcha durumlarını da 403 ile bildiriyor; onları
// süresi dolmuş URL sanmak, rate limitliyken yeniden çözüp tekrar denemek
// demek olurdu.
type Classifier func(resp *http.Response, body []byte) error

type Downloader struct {
	Client *http.Client
	// Logf nil olabilir.
	Logf func(format string, a ...any)
	// Reresolve nil olabilir; nil ise 403/410 kalıcı hata sayılır.
	Reresolve Reresolver
	// Classify nil olabilir.
	Classify Classifier
	// UserAgent, transfer isteklerine de uygulanır. Resolver'ın User-Agent'ı
	// yalnızca API çağrılarını kapsadığı için burada ayrıca verilmesi gerekiyor.
	UserAgent string

	// claimed, aynı koşuda aynı klasörde aynı adı iki kez kullanmayı önler.
	// bunkr albümlerinde yinelenen ad yaygın.
	claimed map[string]bool
}

func (d *Downloader) logf(format string, a ...any) {
	if d.Logf != nil {
		d.Logf(format, a...)
	}
}

func (d *Downloader) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return http.DefaultClient
}

// ErrSHA256Mismatch, indirilen içerik sitenin verdiği hash ile uyuşmadığında döner.
var ErrSHA256Mismatch = errors.New("sha256 uyuşmuyor")

// ErrIncomplete, gövde beklenen boyuttan kısa geldiğinde döner.
var ErrIncomplete = errors.New("indirme eksik")

// Download, tek bir item'ı indirir ve devam edebilirliği yönetir.
//
// Dönüş nil ise dosya nihai adıyla ve doğrulanmış halde diskte demektir.
// ctx iptal edilirse `.part` ve `.part.state` tutarlı halde bırakılır ve
// context hatası döner.
func (d *Downloader) Download(ctx context.Context, outRoot string, it site.Item) error {
	// Temizleme BURADA yapılıyor, çağıranda değil: ad kuralları diske yazan
	// kodla aynı yerde durmak zorunda, yoksa bir çağıran atlar ve ayrılmış bir
	// aygıt adı veya 255 birim sınırını aşan bir bileşen diske sızar.
	//
	// claim()'den ÖNCE olmak zorunda: ad çakışma haritasının anahtarları diskte
	// gerçekten oluşan adlarla aynı olmalı.
	//
	// Dosya adı 255'e DEĞİL, 255 eksi kendi soneklerimize sığdırılıyor: nihai
	// adın yanına `.part` ve `.part.state` yazıyoruz ve o dosyalar da aynı
	// bileşen sınırına tabi. 255'i nihai ada harcamak, `.part.state`'i 266
	// birime çıkarıp NTFS'in isteği reddetmesi demek. Klasör adında böyle bir
	// sonek olmadığı için o tam sınırı kullanabiliyor.
	name := ComponentLimit(it.Filename, MaxComponentUTF16-utf16Len(stateSuffix))
	if name == "" {
		return fmt.Errorf("dosya adı kullanılamaz: %q (%s)", it.Filename, it.SourcePage)
	}
	it.Filename = name
	it.Dir = Component(it.Dir) // boş kalabilir; kök demek

	dir := filepath.Join(outRoot, it.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("klasör açılamadı: %w", err)
	}

	final := filepath.Join(dir, d.claim(dir, it))
	part := final + partSuffix
	statePath := final + stateSuffix

	// Zaten tamamlanmışsa dokunma. done.jsonl adım 9'da gelecek; bu yalnızca
	// aynı koşuda iki kez indirmeyi önleyen ucuz bir kontrol.
	if fi, err := os.Stat(final); err == nil && !fi.IsDir() {
		d.logf("%s zaten var, atlanıyor", filepath.Base(final))
		return nil
	}

	st, hasher, err := d.prepare(part, statePath, it)
	if err != nil {
		return err
	}

	item := it
	tried := false
	for {
		done, newState, aerr := d.attempt(ctx, part, statePath, item, st, hasher)
		if aerr == nil {
			st = newState
			if !done {
				// Sunucu beklenmedik şekilde erken kapattı; kısmi ilerleme
				// kaydedildi, bir üst katman tekrar deneyecek.
				return fmt.Errorf("%w: bağlantı erken kapandı, %d bayt kaydedildi",
					ErrIncomplete, st.Offset)
			}
			break
		}

		var expired *urlExpiredError
		if errors.As(aerr, &expired) && d.Reresolve != nil && !tried {
			// İmzalı URL süresi dolmuş. Bir KEZ yeniden çöz.
			tried = true
			d.logf("imzalı URL %d döndü, yeniden çözülüyor: %s", expired.status, item.SourcePage)
			fresh, rerr := d.Reresolve(ctx, item.SourcePage)
			if rerr != nil {
				return fmt.Errorf("yeniden çözümleme başarısız: %w", rerr)
			}
			// Ad ve klasör korunur; yalnızca indirme adresi tazelenir.
			item.URL = fresh.URL
			if item.SHA256 == "" {
				item.SHA256 = fresh.SHA256
			}
			// Hash ve offset korunur: aynı içeriğe devam ediyoruz.
			st, hasher, err = d.prepare(part, statePath, item)
			if err != nil {
				return err
			}
			continue
		}
		return aerr
	}

	// Beklenen boyut biliniyorsa eksik dosya başarı sayılmaz. Content-Length
	// yoksa resolver'ın bildirdiği boyut yedek olarak devreye girer; aksi halde
	// kırpılmış bir gövde "tamamlandı" sayılırdı.
	if total := st.expectedTotal(); total >= 0 && st.Offset != total {
		return fmt.Errorf("%w: %d/%d bayt (%s)", ErrIncomplete, st.Offset, total, filepath.Base(final))
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	if item.SHA256 != "" && !strings.EqualFold(sum, item.SHA256) {
		// Bozuk `.part` diskte bırakılırsa her koşu aynı hatayı tekrarlar ve
		// item elle silinmeden kurtarılamaz. Temizleyip baştan indirilebilir bırak.
		_ = os.Remove(part)
		_ = os.Remove(statePath)
		return fmt.Errorf("%w: beklenen %s, hesaplanan %s (%s) — .part silindi, tekrar denenebilir",
			ErrSHA256Mismatch, item.SHA256, sum, filepath.Base(final))
	}
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	_ = os.Remove(statePath)
	return nil
}

// prepare, state ile `.part` dosyasını karşılaştırır ve tutarlı bir
// (state, hasher) çifti döndürür.
//
// Üç tutarsızlık var ve üçü de sessizce bozuk dosya üretebilir:
//   - `.part` yok ama state offset > 0  -> resume, dosyanın başına sıfır deliği açar
//   - `.part` state'ten kısa            -> aynı delik, daha küçük
//   - `.part` state'ten uzun            -> fazlalık hash'e girmemiş, kırpılmalı
//
// İlk ikisinde tek güvenli davranış baştan başlamak. Sessiz değil: log basılır.
func (d *Downloader) prepare(part, statePath string, it site.Item) (State, hash.Hash, error) {
	st := loadState(statePath)
	if st.ItemSize < 0 && it.Size > 0 {
		st.ItemSize = it.Size
	}

	reset := func(reason string) (State, hash.Hash, error) {
		if reason != "" {
			d.logf("%s: baştan indiriliyor", reason)
		}
		if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
			return State{}, nil, err
		}
		_ = os.Remove(statePath)
		fresh := freshState()
		if it.Size > 0 {
			fresh.ItemSize = it.Size
		}
		return fresh, sha256.New(), nil
	}

	if st.Offset <= 0 {
		return reset("")
	}

	fi, err := os.Stat(part)
	switch {
	case os.IsNotExist(err):
		// En sinsi durum: hash durumu state'te duruyor, dosya yok. Resume
		// edilirse delik açılır ve sha256 kontrolü GEÇER.
		return reset(fmt.Sprintf(".part yok ama state %d bayt diyor", st.Offset))
	case err != nil:
		return State{}, nil, err
	case fi.Size() < st.Offset:
		return reset(fmt.Sprintf(".part %d bayt, state %d diyor", fi.Size(), st.Offset))
	case fi.Size() > st.Offset:
		// Fazlalık hash'e girmemiş; kırpmak doğru ve güvenli.
		if err := os.Truncate(part, st.Offset); err != nil {
			return State{}, nil, err
		}
	}

	h, err := restoreHasher(part, st)
	if err != nil {
		return reset(fmt.Sprintf("hash durumu kurtarılamadı: %v", err))
	}
	return st, h, nil
}

// urlExpiredError, 403/410'u diğer hatalardan ayırır.
type urlExpiredError struct{ status int }

func (e *urlExpiredError) Error() string { return fmt.Sprintf("imzalı URL geçersiz: %d", e.status) }

// attempt, tek bir HTTP denemesi yapar ve state'i günceller.
// done=true ise dosya tamamlanmıştır.
func (d *Downloader) attempt(
	ctx context.Context, part, statePath string,
	it site.Item, st State, hasher hash.Hash,
) (bool, State, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, it.URL, nil)
	if err != nil {
		return false, st, err
	}
	if d.UserAgent != "" {
		req.Header.Set("User-Agent", d.UserAgent)
	}
	// Item.Headers politikadan türer (örn. bunkr'da zorunlu item sayfası
	// Referer'ı) ve User-Agent'ı ezebilir.
	for k, v := range it.Headers {
		req.Header.Set(k, v)
	}

	// Ayrı bir Accept-Ranges probu YOK: round-trip israfı ve garanti değil.
	// Tek doğru sinyal gerçek isteğin status kodudur.
	if st.resumable() {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(st.Offset, 10)+"-")
		req.Header.Set("If-Range", st.Validator)
	}

	resp, err := d.client().Do(req)
	if err != nil {
		return false, st, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Kaynak değişmiş ya da sunucu Range desteklemiyor: baştan yaz.
		if st.Offset > 0 {
			d.logf("sunucu 200 döndü, kaynak değişmiş olabilir: baştan indiriliyor")
		}
		st = mergeServerState(st, resp)
		hasher.Reset()
		if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
			return false, st, err
		}
		st.Offset = 0
		return d.stream(ctx, part, statePath, resp.Body, st, hasher)

	case http.StatusPartialContent:
		start, total, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if perr != nil {
			return false, st, fmt.Errorf("Content-Range ayrıştırılamadı: %w", perr)
		}
		if start != st.Offset {
			// Sunucu istediğimiz yerden başlamadı. Yapıştırmak bozuk dosya üretir.
			return false, st, fmt.Errorf("Content-Range %d'den başlıyor, %d bekleniyordu", start, st.Offset)
		}
		if total >= 0 {
			st.TotalSize = total
		}
		return d.stream(ctx, part, statePath, resp.Body, st, hasher)

	case http.StatusRequestedRangeNotSatisfiable:
		// 416 "tamamlandı" DEMEK DEĞİL; yalnızca "offset >= mevcut uzunluk"
		// demek. Ayrımı state yapar.
		if total := st.expectedTotal(); total >= 0 && st.Offset == total {
			d.logf("416: indirme zaten tamamlanmış, doğrulanıyor")
			return true, st, nil
		}
		d.logf("416 ama offset=%d beklenen=%d: .part bozuk, sıfırlanıyor", st.Offset, st.expectedTotal())
		if err := os.Remove(part); err != nil && !os.IsNotExist(err) {
			return false, st, err
		}
		_ = os.Remove(statePath)
		hasher.Reset()
		st = freshState()
		st.ItemSize = it.Size
		return false, st, errors.New("range reddedildi, .part sıfırlandı; tekrar deneyin")

	case http.StatusForbidden, http.StatusGone:
		// Siteye özgü sınıflandırıcı varsa önce ona sor: 403 her zaman
		// "süresi dolmuş imzalı URL" değildir.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		if d.Classify != nil {
			if cerr := d.Classify(resp, body); cerr != nil {
				return false, st, cerr
			}
		}
		return false, st, &urlExpiredError{status: resp.StatusCode}

	default:
		return false, st, fmt.Errorf("%s: HTTP %s", it.URL, resp.Status)
	}
}

// stream, gövdeyi `.part`'a yazar ve hash'i ilerletir.
func (d *Downloader) stream(
	ctx context.Context, part, statePath string,
	body io.Reader, st State, hasher hash.Hash,
) (bool, State, error) {
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, st, err
	}
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		f.Close()
		return false, st, err
	}

	buf := make([]byte, 256<<10)
	for {
		select {
		case <-ctx.Done():
			_ = f.Sync()
			f.Close()
			saveState(statePath, st, hasher)
			return false, st, ctx.Err()
		default:
		}

		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				_ = f.Sync()
				f.Close()
				saveState(statePath, st, hasher)
				return false, st, werr
			}
			hasher.Write(buf[:n])
			st.Offset += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = f.Sync()
			f.Close()
			saveState(statePath, st, hasher)
			return false, st, rerr
		}
	}

	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		saveState(statePath, st, hasher)
		return false, st, err
	}

	// Beklenen boyut biliniyorsa eksik veri sessizce başarı sayılmaz.
	if total := st.expectedTotal(); total >= 0 && st.Offset < total {
		saveState(statePath, st, hasher)
		return false, st, nil
	}
	return true, st, nil
}

// mergeServerState, 200 yanıtından taze sunucu bilgisi alır ve validator'ı seçer.
// Resolver kaynaklı ItemSize korunur.
//
// Zayıf ETag (W/ öneki) If-Range'de KULLANILAMAZ (RFC 7232): zayıf validator
// "anlamca eşdeğer" demek, "bayt bayt aynı" demek değil. Kullanırsan tam olarak
// kapatmaya çalıştığın bozuk-dosya sınıfını geri getirirsin.
func mergeServerState(prev State, resp *http.Response) State {
	st := freshState()
	st.ItemSize = prev.ItemSize
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			st.TotalSize = n
		}
	}
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	switch {
	case etag != "" && !strings.HasPrefix(etag, "W/"):
		st.Validator, st.ValidatorType = etag, ValidatorETag
	case resp.Header.Get("Last-Modified") != "":
		st.Validator, st.ValidatorType = resp.Header.Get("Last-Modified"), ValidatorLastModified
	default:
		// İkisi de yoksa resume denenmez.
		st.Validator, st.ValidatorType = "", ValidatorNone
	}
	return st
}

// parseContentRange, "bytes 100-199/1234" biçimini çözer.
// Toplam bilinmiyorsa ("*") -1 döner.
func parseContentRange(v string) (start, total int64, err error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, -1, fmt.Errorf("beklenmeyen biçim: %q", v)
	}
	v = strings.TrimPrefix(v, "bytes ")
	rangePart, totalPart, ok := strings.Cut(v, "/")
	if !ok {
		return 0, -1, fmt.Errorf("beklenmeyen biçim: %q", v)
	}
	startStr, _, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, -1, fmt.Errorf("beklenmeyen aralık: %q", rangePart)
	}
	start, err = strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	if err != nil {
		return 0, -1, err
	}
	total = -1
	if t := strings.TrimSpace(totalPart); t != "*" {
		total, err = strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0, -1, err
		}
	}
	return start, total, nil
}

func loadState(path string) State {
	data, err := os.ReadFile(path)
	if err != nil {
		return freshState()
	}
	st := freshState()
	if json.Unmarshal(data, &st) != nil || st.Offset < 0 {
		return freshState()
	}
	return st
}

// saveState, state'i atomik yazar: yarım yazılmış bir state dosyası
// bir sonraki koşuda sessizce yanlış offset demek olurdu.
func saveState(path string, st State, hasher hash.Hash) {
	if m, ok := hasher.(encoding.BinaryMarshaler); ok {
		if b, err := m.MarshalBinary(); err == nil {
			st.SHA256State = b
		}
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// restoreHasher, kaydedilmiş sha256 durumunu geri yükler.
// Durum yoksa `.part`'ı offset'e kadar yeniden okuyup hash'i kurar.
//
// Çağıran, `.part`'ın state ile tutarlı olduğunu ÖNCEDEN doğrulamak zorunda
// (bkz. prepare): burada dosya kısa olsa bile CopyN hata verir, ama hash
// durumu state'ten gelirse dosya hiç okunmaz ve tutarsızlık sessiz kalır.
func restoreHasher(part string, st State) (hash.Hash, error) {
	h := sha256.New()
	if st.Offset == 0 {
		return h, nil
	}
	if len(st.SHA256State) > 0 {
		if u, ok := h.(encoding.BinaryUnmarshaler); ok {
			if err := u.UnmarshalBinary(st.SHA256State); err == nil {
				return h, nil
			}
		}
		h = sha256.New()
	}
	f, err := os.Open(part)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := io.CopyN(h, f, st.Offset); err != nil {
		return nil, err
	}
	return h, nil
}

// claim, aynı klasörde aynı adın iki kez kullanılmasını önler.
// Sonek Index'ten türer, böylece koşular arasında deterministiktir: aynı albüm
// aynı sırada çözüldüğü sürece aynı item aynı adı alır. Üretilen adın kendisi
// de çakışabileceği için boş bir ad bulunana kadar ilerlenir.
func (d *Downloader) claim(dir string, it site.Item) string {
	if d.claimed == nil {
		d.claimed = map[string]bool{}
	}
	key := func(n string) string { return strings.ToLower(filepath.Join(dir, n)) }

	if !d.claimed[key(it.Filename)] {
		d.claimed[key(it.Filename)] = true
		return it.Filename
	}
	ext := filepath.Ext(it.Filename)
	base := strings.TrimSuffix(it.Filename, ext)
	for n := it.Index + 1; ; n++ {
		alt := fmt.Sprintf("%s (%d)%s", base, n, ext)
		if !d.claimed[key(alt)] {
			d.claimed[key(alt)] = true
			return alt
		}
	}
}
