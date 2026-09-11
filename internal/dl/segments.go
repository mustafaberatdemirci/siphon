package dl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// Parçalı çok bağlantılı indirme: tek dosya N bayt aralığına bölünür, her
// aralık ayrı bir bağlantıyla çekilip önceden boyutlandırılmış .part
// dosyasının kendi konumuna yazılır.
//
// Ne zaman DEVREYE GİRMEZ (kendiliğinden tek akışa düşer):
//   - boyut bilinmiyorsa (Content-Length yok): aralık kurulamaz
//   - sunucu Range'i yok sayıp 200 dönerse
//   - dosya MinSegmentSize'dan küçükse: bölmenin maliyeti kazancını aşar
//   - akış çözücüsü varsa (mega): AES-CTR parça parça çözülebilir ama
//     bütünlük MAC'i sıralı; parçalı mega ayrı bir iş. mega zaten kota
//     sınırlı, hız sınırlı değil.
//   - tek akışlı bir .part yarım kalmışsa: o kaldığı yerden tek akışla biter
//
// Bütünlük: sha256 sıralı bir hash olduğu için parçalar bitince dosya BAŞTAN
// okunup hesaplanıyor. Bir tam yerel okuma; SSD'de saniyeler, sessizce
// "doğru" saymaktan çok daha ucuz.

// Segment, dosyanın bir bayt aralığı ve o aralıkta ne kadarının indiği.
type Segment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // hariç
	Done  int64 `json:"done"`
}

// DefaultMinSegmentSize, altında bölme yapılmayan dosya boyutu.
const DefaultMinSegmentSize = 8 << 20

// segmentStateEvery, parça durumunun diske en sık yazılma aralığı.
const segmentStateEvery = time.Second

// errNoRangeSupport, sunucunun aralık isteğini yok saydığını söyler; tek
// akışa düşülür.
var errNoRangeSupport = errors.New("sunucu Range desteklemiyor")

// errSourceChanged, devam sırasında sunucunun If-Range'i reddedip 200
// döndüğünü söyler: parçalar artık aynı sürüme ait değil.
var errSourceChanged = errors.New("kaynak değişmiş, parçalı durum geçersiz")

// Parça başına yeniden deneme sınırları. Dış politika (Worker) tüm indirmeyi
// zaten yeniden deniyor; bu, tek bir parçanın geçici hatasının diğer üç
// parçayı düşürmemesi için.
const (
	segmentTries       = 6
	segmentBackoffBase = 500 * time.Millisecond
	segmentBackoffMax  = 8 * time.Second
)

// shrinkSem, kapasitesi koşu sırasında DÜŞÜRÜLEBİLEN bir semafor.
//
// ÖLÇÜLDÜ (bunkr CDN, 2026-09-11): aynı dosyaya 4 paralel aralık isteğinden
// biri 503 alıyor; sıralı istekler sorunsuz. Yani sunucu bağlantı sayısını
// sınırlıyor. Bir parça 503/429 alınca kapasite bir düşürülüyor: kalan
// parçalar daha az bağlantıyla sürüyor, indirme tamamen düşmüyor. IDM'in
// yaptığı da bu.
type shrinkSem struct {
	mu   sync.Mutex
	cond *sync.Cond
	cap  int
	used int
}

func newShrinkSem(ctx context.Context, n int) *shrinkSem {
	s := &shrinkSem{cap: n}
	s.cond = sync.NewCond(&s.mu)
	// İptal bekleyenleri uyandırmalı; sync.Cond context bilmez.
	go func() {
		<-ctx.Done()
		s.cond.Broadcast()
	}()
	return s
}

func (s *shrinkSem) acquire(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.used >= s.cap {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.cond.Wait()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.used++
	return nil
}

func (s *shrinkSem) release() {
	s.mu.Lock()
	s.used--
	s.mu.Unlock()
	s.cond.Broadcast()
}

// shrink, kapasiteyi bir düşürür (en az 1). Düştüyse true.
func (s *shrinkSem) shrink() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cap <= 1 {
		return false
	}
	s.cap--
	return true
}

func (s *shrinkSem) capacity() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cap
}

// planSegments, [0,size) aralığını n eşit parçaya böler. Son parça artığı alır.
func planSegments(size int64, n int) []Segment {
	if n < 1 {
		n = 1
	}
	if int64(n) > size {
		n = int(size)
	}
	if n < 1 {
		return []Segment{{Start: 0, End: size}}
	}
	each := size / int64(n)
	out := make([]Segment, n)
	for i := range out {
		out[i].Start = int64(i) * each
		out[i].End = out[i].Start + each
	}
	out[n-1].End = size
	return out
}

// segmentsFor, bu item için etkin parça sayısı.
func (d *Downloader) segmentsFor(it site.Item) int {
	if d.Decode != nil || d.Segments < 2 {
		return 1
	}
	min := d.MinSegmentSize
	if min <= 0 {
		min = DefaultMinSegmentSize
	}
	if it.Size > 0 && it.Size < min {
		return 1
	}
	return d.Segments
}

// segmented, parçalı indirmeyi baştan sona yürütür.
//
// Dönüş: (sonuç, nil) tamam; (Result{}, errNoRangeSupport) tek akışa düş;
// başka hata: çağıran (politika) yeniden dener, parça durumu diskte.
func (d *Downloader) segmented(ctx context.Context, final, part, statePath string, it site.Item, want int) (Result, error) {
	st := loadState(statePath)

	// Devam edilebilir bir parçalı durum var mı?
	resuming := false
	if len(st.Segments) > 0 {
		fi, err := os.Stat(part)
		switch {
		case err != nil, fi.Size() != st.TotalSize, st.Validator == "", st.ValidatorType == ValidatorNone:
			// Dosya yok, boyutu tutmuyor ya da doğrulayıcı yok: sıfırdan.
			d.logf("parçalı durum kullanılamaz, baştan indiriliyor")
			st = freshState()
		default:
			resuming = true
		}
	} else if st.Offset > 0 {
		// Tek akışlı yarım dosya: ona parçalı devam edilmez, çağıran tek
		// akışla bitirir.
		return Result{}, errNoRangeSupport
	}

	item := it
	reresolved := false
	for {
		res, err := d.segmentedOnce(ctx, final, part, statePath, item, want, &st, resuming)
		var expired *urlExpiredError
		if errors.As(err, &expired) && d.Reresolve != nil && !reresolved {
			reresolved = true
			d.logf("imzalı URL %d döndü, yeniden çözülüyor: %s", expired.status, item.SourcePage)
			fresh, rerr := d.Reresolve(ctx, item.SourcePage)
			if rerr != nil {
				return Result{}, fmt.Errorf("yeniden çözümleme başarısız: %w", rerr)
			}
			item.URL = fresh.URL
			if item.SHA256 == "" {
				item.SHA256 = fresh.SHA256
			}
			resuming = len(st.Segments) > 0
			continue
		}
		return res, err
	}
}

func (d *Downloader) segmentedOnce(ctx context.Context, final, part, statePath string, it site.Item, want int, st *State, resuming bool) (Result, error) {
	target := it.URL
	if d.PrepareURL != nil {
		prepared, perr := d.PrepareURL(ctx, target)
		if perr != nil {
			return Result{}, Retryable(perr)
		}
		target = prepared
	}

	if !resuming {
		// Sonda: boyut, doğrulayıcı ve Range desteği tek küçük istekle.
		size, validator, vtype, err := d.probe(ctx, target, it)
		if err != nil {
			return Result{}, err
		}
		if size <= 0 || validator == "" || vtype == ValidatorNone {
			// Boyutsuz ya da doğrulayıcısız dosya parçalanamaz: parçaların
			// aynı sürüme ait olduğunu garanti edemeyiz.
			return Result{}, errNoRangeSupport
		}
		min := d.MinSegmentSize
		if min <= 0 {
			min = DefaultMinSegmentSize
		}
		if size < min {
			return Result{}, errNoRangeSupport
		}
		*st = freshState()
		st.TotalSize, st.ItemSize = size, it.Size
		st.Validator, st.ValidatorType = validator, vtype
		st.Segments = planSegments(size, want)

		f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return Result{}, err
		}
		if err := f.Truncate(size); err != nil {
			f.Close()
			return Result{}, err
		}
		f.Close()
		saveState(statePath, *st, nil)
	}

	// Ek bağlantı yuvaları: host sınırı bağlantı sayısını sınırlıyor.
	extra := len(st.Segments) - 1
	release := func() {}
	if extra > 0 && d.AcquireExtra != nil {
		var got int
		got, release = d.AcquireExtra(target, extra)
		if got < extra {
			d.logf("host sınırı: %d yerine %d ek bağlantı", extra, got)
		}
		extra = got
	}
	defer release()
	workers := extra + 1
	d.logf("%s: parçalı indirme, %d bağlantı, %d parça, %s", filepath.Base(final), workers, len(st.Segments), humanSize(st.TotalSize))

	f, err := os.OpenFile(part, os.O_WRONLY, 0o644)
	if err != nil {
		return Result{}, err
	}
	// Windows açık dosyayı yeniden adlandırmaz: hash ve rename'den ÖNCE
	// kapatılmak zorunda. closeOnce her yoldan bir kez kapatır.
	var closeOnce sync.Once
	closeF := func() { closeOnce.Do(func() { _ = f.Sync(); _ = f.Close() }) }
	defer closeF()

	var (
		mu        sync.Mutex
		lastSave  = time.Now()
		lastRep   = time.Now()
		total     = st.TotalSize
		segs      = st.Segments
		doneBytes = func() int64 {
			var n int64
			for _, s := range segs {
				n += s.Done
			}
			return n
		}
	)
	snapshot := func() State {
		c := *st
		c.Segments = append([]Segment(nil), segs...)
		c.Offset = 0
		return c
	}
	// tick, her parça yazımından sonra çağrılır: ilerlemeyi bildirir ve
	// parça durumunu periyodik olarak diske yazar. Kayıt İNDİRME SIRASINDA
	// yapılmalı; yalnızca sonda yazılsaydı süreç ortada ölünce her şey
	// sıfırdan başlardı.
	tick := func(force bool) {
		mu.Lock()
		now := time.Now()
		doRep := force || now.Sub(lastRep) >= progressInterval
		doSave := force || now.Sub(lastSave) >= segmentStateEvery
		if doRep {
			lastRep = now
		}
		if doSave {
			lastSave = now
		}
		n := doneBytes()
		var snap State
		if doSave {
			snap = snapshot()
		}
		mu.Unlock()
		if doSave {
			saveState(statePath, snap, nil)
		}
		if doRep && d.Progress != nil {
			d.Progress(it, n, total)
		}
	}
	tick(true)

	g, gctx := errgroup.WithContext(ctx)
	sem := newShrinkSem(gctx, workers)
	for i := range segs {
		i := i
		if segs[i].Done >= segs[i].End-segs[i].Start {
			continue
		}
		g.Go(func() error {
			return d.runSegment(gctx, sem, target, it, *st, f, &mu, &segs[i], tick)
		})
	}
	gerr := g.Wait()
	closeF()
	tick(true)

	if errors.Is(gerr, errSourceChanged) {
		// Sunucu dosyayı değiştirmiş: eski parçalar başka bir sürüme ait.
		// Durum silinmezse bir sonraki deneme aynı If-Range ile aynı 200'ü
		// alır ve bütçe bitene kadar döner.
		_ = os.Remove(part)
		_ = os.Remove(statePath)
		return Result{}, Retryable(gerr)
	}
	if gerr != nil {
		return Result{}, gerr
	}
	if got := doneBytes(); got != total {
		return Result{}, Retryable(fmt.Errorf("%w: %d/%d bayt (%s)", ErrIncomplete, got, total, filepath.Base(final)))
	}

	// Bütünlük: sha256 sıralı, dosya baştan okunuyor.
	sum, err := hashFile(part)
	if err != nil {
		return Result{}, err
	}
	if it.SHA256 != "" && !strings.EqualFold(sum, it.SHA256) {
		_ = os.Remove(part)
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("%w: beklenen %s, hesaplanan %s (%s) — .part silindi, tekrar denenebilir",
			ErrSHA256Mismatch, it.SHA256, sum, filepath.Base(final))
	}
	if err := os.Rename(part, final); err != nil {
		return Result{}, fmt.Errorf("rename: %w", err)
	}
	_ = os.Remove(statePath)
	return Result{Path: final, Size: total, SHA256: sum}, nil
}

// probe, ilk baytı isteyerek boyutu, doğrulayıcıyı ve Range desteğini öğrenir.
func (d *Downloader) probe(ctx context.Context, target string, it site.Item) (size int64, validator, vtype string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", "", err
	}
	d.setHeaders(req, it)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := d.Client.Do(req)
	if err != nil {
		return 0, "", "", Retryable(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if verr := d.validate(resp); verr != nil {
			return 0, "", "", verr
		}
		_, total, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if perr != nil || total <= 0 {
			return 0, "", "", errNoRangeSupport
		}
		v, vt := pickValidator(resp)
		return total, v, vt, nil
	case http.StatusOK:
		// Aralık yok sayıldı.
		return 0, "", "", errNoRangeSupport
	default:
		return 0, "", "", d.classifyFailure(resp)
	}
}

// runSegment, bir parçayı bitene kadar sürer: geçici hatada bekleyip
// yeniden dener, sunucu bağlantı sayısından şikâyet ederse (503/429)
// paralelliği düşürür. Kalıcı hatalar (kaynak değişti, adres süresi doldu,
// 4xx) hemen döner ve grubu düşürür.
func (d *Downloader) runSegment(ctx context.Context, sem *shrinkSem, target string, it site.Item, st State, f *os.File, mu *sync.Mutex, seg *Segment, tick func(bool)) error {
	for attempt := 0; ; attempt++ {
		if err := sem.acquire(ctx); err != nil {
			return err
		}
		err := d.fetchSegment(ctx, target, it, st, f, mu, seg, tick)
		sem.release()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var rt retryableError
		if !errors.As(err, &rt) || !rt.Retryable() || attempt+1 >= segmentTries {
			return err
		}
		if isOverloaded(err) {
			if sem.shrink() {
				d.logf("%s: sunucu bağlantı sayısından şikâyetçi (%v); paralellik %d'e düşürüldü",
					filepath.Base(it.Filename), firstLineOf(err), sem.capacity())
			}
		}
		wait := segmentBackoffBase << uint(attempt)
		if wait > segmentBackoffMax {
			wait = segmentBackoffMax
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

type retryableError interface{ Retryable() bool }

// overloadedError, sunucunun "çok fazla bağlantı" dediği durumlar: 503 ve 429.
type overloadedError struct{ status int }

func (e *overloadedError) Error() string   { return fmt.Sprintf("HTTP %d", e.status) }
func (e *overloadedError) Retryable() bool { return true }
func isOverloaded(err error) bool          { var o *overloadedError; return errors.As(err, &o) }
func firstLineOf(err error) string         { return firstLine(err.Error()) }

// firstLine, çok satırlı hata mesajının ilk satırı (log satırı tek satır kalsın).
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// fetchSegment, tek bir aralığı kaldığı yerden çeker ve dosyaya yazar.
func (d *Downloader) fetchSegment(ctx context.Context, target string, it site.Item, st State, f *os.File, mu *sync.Mutex, seg *Segment, report func(bool)) error {
	mu.Lock()
	start := seg.Start + seg.Done
	end := seg.End
	mu.Unlock()
	if start >= end {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	d.setHeaders(req, it)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1))
	if st.Validator != "" {
		// Parçalar aynı sürüme ait olmak ZORUNDA; sunucu dosyayı değiştirdiyse
		// 200 döner ve aşağıda yakalanır.
		req.Header.Set("If-Range", st.Validator)
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return Retryable(err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		gotStart, _, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if perr != nil || gotStart != start {
			return fmt.Errorf("parça %d-%d: Content-Range %d'den başlıyor", start, end-1, gotStart)
		}
	case http.StatusOK:
		// Kaynak değişmiş ya da Range yok sayıldı: parçaları karıştırmak
		// bozuk dosya üretir. Çağıran durumu silip baştan başlatır.
		return errSourceChanged
	case http.StatusRequestedRangeNotSatisfiable:
		return fmt.Errorf("parça %d-%d: 416", start, end-1)
	default:
		return d.classifyFailure(resp)
	}

	buf := make([]byte, 256<<10)
	pos := start
	for pos < end {
		want := len(buf)
		if rem := end - pos; rem < int64(want) {
			want = int(rem)
		}
		want = d.Throttle.chunkFor(want)
		n, rerr := resp.Body.Read(buf[:want])
		if n > 0 {
			if terr := d.Throttle.Wait(ctx, n); terr != nil {
				return terr
			}
			if _, werr := f.WriteAt(buf[:n], pos); werr != nil {
				return werr
			}
			pos += int64(n)
			mu.Lock()
			seg.Done = pos - seg.Start
			mu.Unlock()
			report(false)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return Retryable(rerr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if pos < end {
		return Retryable(fmt.Errorf("%w: parça %d-%d %d baytta kesildi", ErrIncomplete, start, end-1, pos))
	}
	return nil
}

// classifyFailure, parça isteklerindeki 2xx dışı durumları hataya çevirir.
// attempt'teki kurallarla aynı: 403/410 sınıflandırıcıya sorulur, yoksa
// "adres süresi dolmuş"; 5xx/429 geçici; diğer 4xx kalıcı.
func (d *Downloader) classifyFailure(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if d.Classify != nil {
		if cerr := d.Classify(resp, body); cerr != nil {
			return cerr
		}
	}
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone:
		return &urlExpiredError{status: resp.StatusCode}
	case resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests:
		// Sunucu yük/bağlantı sayısından şikâyetçi: geçici VE paralelliği
		// düşürme sinyali.
		return &overloadedError{status: resp.StatusCode}
	case resp.StatusCode >= 500:
		return Retryable(fmt.Errorf("HTTP %s", resp.Status))
	default:
		return fmt.Errorf("HTTP %s", resp.Status)
	}
}

// setHeaders, item'ın başlıklarını ve User-Agent'ı isteğe uygular.
func (d *Downloader) setHeaders(req *http.Request, it site.Item) {
	if d.UserAgent != "" {
		req.Header.Set("User-Agent", d.UserAgent)
	}
	for k, v := range it.Headers {
		req.Header.Set(k, v)
	}
}

// hashFile, dosyanın sha256'sını baştan okuyarak hesaplar.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// humanSize, log için kaba boyut.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
