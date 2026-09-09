// Package dl, devam edebilir indirmeden sorumludur.
//
// Tek doğruluk kaynağı `.part.state` dosyasıdır, `.part`'ın boyutu DEĞİL.
// Çökme anında ikisi ayrışır: işletim sistemi `.part`'a yazmış olabilir ama
// state güncellenmemiştir. Boyuta güvenirsen sha256 sessizce yanlış çıkar ve
// bu, aracın tüm amacı olan "sessiz başarısızlık yok" ilkesinin ihlalidir.
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
	TotalSize     int64  `json:"total_size"` // bilinmiyorsa -1
}

// resumable, bu state ile Range denemenin anlamlı olup olmadığını söyler.
// Validator yoksa resume DENENMEZ: sunucu dosyayı değiştirmişse iki farklı
// dosyanın parçalarını birbirine yapıştırırız ve bunu fark etmeyiz.
func (s State) resumable() bool {
	return s.Offset > 0 && s.Validator != "" && s.ValidatorType != ValidatorNone && s.TotalSize >= 0
}

// Reresolver, imzalı URL süresi dolduğunda (403/410) tek item'ı yeniden çözer.
type Reresolver func(ctx context.Context, sourcePage string) (site.Item, error)

type Downloader struct {
	Client *http.Client
	// Logf nil olabilir.
	Logf func(format string, a ...any)
	// Reresolve nil olabilir; nil ise 403/410 kalıcı hata sayılır.
	Reresolve Reresolver

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

// Download, tek bir item'ı indirir ve devam edebilirliği yönetir.
//
// Dönüş nil ise dosya nihai adıyla ve doğrulanmış halde diskte demektir.
// ctx iptal edilirse `.part` ve `.part.state` tutarlı halde bırakılır ve
// context hatası döner.
func (d *Downloader) Download(ctx context.Context, outRoot string, it site.Item) error {
	if it.Filename == "" {
		return fmt.Errorf("dosya adı boş: %s", it.SourcePage)
	}
	dir := filepath.Join(outRoot, it.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("klasör açılamadı: %w", err)
	}

	final := filepath.Join(dir, d.claim(dir, it))
	part := final + ".part"
	statePath := part + ".state"

	// Zaten tamamlanmışsa dokunma. done.jsonl adım 9'da gelecek; bu yalnızca
	// aynı koşuda iki kez indirmeyi önleyen ucuz bir kontrol.
	if fi, err := os.Stat(final); err == nil && !fi.IsDir() {
		d.logf("%s zaten var, atlanıyor", filepath.Base(final))
		return nil
	}

	st := loadState(statePath)

	// `.part`'ı state'in söylediği offset'e kırp. Dosya boyutu doğruluk
	// kaynağı DEĞİL; çökme anında ikisi ayrışır.
	if err := truncatePart(part, st.Offset); err != nil {
		return err
	}
	if st.Offset == 0 {
		st = State{TotalSize: -1}
	}

	hasher, err := restoreHasher(part, st)
	if err != nil {
		// Hash durumu kurtarılamıyorsa baştan başlamak tek güvenli seçenek.
		d.logf("hash durumu kurtarılamadı, baştan: %v", err)
		_ = os.Remove(part)
		_ = os.Remove(statePath)
		st = State{TotalSize: -1}
		hasher = sha256.New()
	}

	item := it
	tried := false
	for {
		done, newState, err := d.attempt(ctx, part, statePath, item, st, hasher)
		if err == nil {
			st = newState
			if !done {
				// Sunucu beklenmedik şekilde erken kapattı; kısmi ilerleme
				// kaydedildi, bir üst katman tekrar deneyecek.
				return fmt.Errorf("bağlantı erken kapandı, %d bayt kaydedildi", st.Offset)
			}
			break
		}

		var expired *urlExpiredError
		if errors.As(err, &expired) && d.Reresolve != nil && !tried {
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
			st, hasher, err = reloadFor(part, statePath, st)
			if err != nil {
				return err
			}
			continue
		}
		return err
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	if item.SHA256 != "" && !strings.EqualFold(sum, item.SHA256) {
		return fmt.Errorf("%w: beklenen %s, hesaplanan %s (%s)",
			ErrSHA256Mismatch, item.SHA256, sum, filepath.Base(final))
	}
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	_ = os.Remove(statePath)
	return nil
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
		st = newStateFrom(resp)
		hasher.Reset()
		if err := truncatePart(part, 0); err != nil {
			return false, st, err
		}
		return d.stream(ctx, part, statePath, resp.Body, st, hasher)

	case http.StatusPartialContent:
		start, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return false, st, fmt.Errorf("Content-Range ayrıştırılamadı: %w", err)
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
		if st.TotalSize >= 0 && st.Offset == st.TotalSize {
			d.logf("416: indirme zaten tamamlanmış, doğrulanıyor")
			return true, st, nil
		}
		d.logf("416 ama offset=%d total=%d: .part bozuk, sıfırlanıyor", st.Offset, st.TotalSize)
		if err := truncatePart(part, 0); err != nil {
			return false, st, err
		}
		_ = os.Remove(statePath)
		hasher.Reset()
		st = State{TotalSize: -1}
		return false, st, errors.New("range reddedildi, .part sıfırlandı; tekrar deneyin")

	case http.StatusForbidden, http.StatusGone:
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
			st.Offset = flushAndSync(f, st.Offset)
			f.Close()
			saveState(statePath, st, hasher)
			return false, st, ctx.Err()
		default:
		}

		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Sync()
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
			st.Offset = flushAndSync(f, st.Offset)
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

	// TotalSize biliniyorsa eksik veri sessizce başarı sayılmaz.
	if st.TotalSize >= 0 && st.Offset < st.TotalSize {
		saveState(statePath, st, hasher)
		return false, st, nil
	}
	return true, st, nil
}

func flushAndSync(f *os.File, offset int64) int64 {
	_ = f.Sync()
	return offset
}

// newStateFrom, 200 yanıtından taze state kurar ve validator'ı seçer.
//
// Zayıf ETag (W/ öneki) If-Range'de KULLANILAMAZ (RFC 7232): zayıf validator
// "anlamca eşdeğer" demek, "bayt bayt aynı" demek değil. Kullanırsan tam olarak
// kapatmaya çalıştığın bozuk-dosya sınıfını geri getirirsin.
func newStateFrom(resp *http.Response) State {
	st := State{TotalSize: -1}
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

func truncatePart(part string, offset int64) error {
	fi, err := os.Stat(part)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Size() == offset {
		return nil
	}
	if offset == 0 {
		return os.Remove(part)
	}
	if fi.Size() < offset {
		// Dosya state'in söylediğinden kısa: state'e güvenilemez.
		return fmt.Errorf(".part (%d bayt) state offset'inden (%d) kısa", fi.Size(), offset)
	}
	return os.Truncate(part, offset)
}

func loadState(path string) State {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{TotalSize: -1}
	}
	var st State
	if json.Unmarshal(data, &st) != nil || st.Offset < 0 {
		return State{TotalSize: -1}
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

func reloadFor(part, statePath string, st State) (State, hash.Hash, error) {
	saved := loadState(statePath)
	if saved.Offset > 0 {
		st = saved
	}
	h, err := restoreHasher(part, st)
	return st, h, err
}

// claim, aynı klasörde aynı adın iki kez kullanılmasını önler.
// Sonek Index'ten türetilir, böylece koşular arasında deterministiktir:
// aynı albüm aynı sırada çözüldüğü sürece aynı item aynı adı alır.
func (d *Downloader) claim(dir string, it site.Item) string {
	if d.claimed == nil {
		d.claimed = map[string]bool{}
	}
	name := it.Filename
	key := strings.ToLower(filepath.Join(dir, name))
	if !d.claimed[key] {
		d.claimed[key] = true
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	alt := fmt.Sprintf("%s (%d)%s", base, it.Index+1, ext)
	d.claimed[strings.ToLower(filepath.Join(dir, alt))] = true
	return alt
}
