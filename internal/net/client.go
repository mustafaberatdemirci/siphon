// Package net, HTTP istemcisi fabrikası, host başına eşzamanlılık sınırı ve
// jitter'lı üstel backoff sağlar.
//
// Premise 3'ün somut hali burada: rate limit bir hata değil, normal çalışma
// durumudur. pixeldrain ücretsiz hesapta eşzamanlı bağlantı ve transfer
// limitleri uyguluyor; araç bunlara çarpmayı bekleyip geri çekilmek zorunda,
// yoksa limiti kendi eliyle derinleştirir.
package net

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// NewClient, transfer ve API istekleri için istemciyi kurar.
//
// Client.Timeout KASITLI olarak verilmiyor: o alan gövde okumayı da kapsar ve
// birkaç gigabaytlık bir indirmeyi ortasından keser. Zaman aşımları bağlantı
// kurma ve yanıt başlığı seviyesinde tutuluyor; takılan bir sunucu yakalanır,
// yavaş ama çalışan bir transfer kesilmez.
func NewClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// HostLimiter, host başına eşzamanlı istek sayısını sınırlar.
//
// Sınır HOST başına, koşu başına değil: bir albüm birden fazla CDN host'una
// yayılabiliyor (bunkr'da normal davranış) ve o durumda tek bir genel sayaç
// yanlış yerde daraltma yapar. Tek bir host'a 3 yerine 30 eşzamanlı bağlantı
// açmak pixeldrain'de doğrudan max_concurrent_downloads demek.
//
// x/sync/semaphore alınmadı: host başına tamponlu kanal bu kadar kod.
type HostLimiter struct {
	n  int
	mu sync.Mutex
	ch map[string]chan struct{}
}

func NewHostLimiter(n int) *HostLimiter {
	if n < 1 {
		n = 1
	}
	return &HostLimiter{n: n, ch: make(map[string]chan struct{})}
}

// HostOf, bir URL'in limiter anahtarını döndürür. Ayrıştırılamayan URL'ler
// tek bir kovaya düşer; sessizce sınırsız bırakmaktan iyidir.
func HostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "?"
	}
	return u.Host
}

func (l *HostLimiter) slot(host string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.ch[host]
	if !ok {
		c = make(chan struct{}, l.n)
		l.ch[host] = c
	}
	return c
}

// Acquire, host için bir yuva alır. Dönen fonksiyon yuvayı bırakır ve
// çağrılmak zorundadır (defer).
func (l *HostLimiter) Acquire(ctx context.Context, host string) (func(), error) {
	c := l.slot(host)
	select {
	case c <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-c }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Backoff, jitter'lı üstel geri çekilme üretir.
//
// cenkalti/backoff alınmadı: bu kadar kod bir bağımlılık ve v4/v5 geçişi
// taşımaya değmez.
type Backoff struct {
	Base time.Duration // ilk bekleme
	Max  time.Duration // üst sınır
}

// Delay, attempt (0 tabanlı) için bekleme süresini döndürür.
//
// Yarım jitter kullanılıyor: bekleme [d/2, d) aralığından seçiliyor, d ise
// üstel olarak büyüyor. Tam jitter ([0, d)) sıfıra yakın değerler üretip bir
// denemeyi boşa harcayabiliyor; sabit gecikme ise aynı anda limite çarpan
// istekleri aynı anda geri getirip sürüyü yeniden oluşturuyor. Yarım jitter
// ikisinin arasında: ilerleme garantili, eşzamanlılık dağıtılmış.
func (b Backoff) Delay(attempt int) time.Duration {
	base := b.Base
	if base <= 0 {
		base = time.Second
	}
	max := b.Max
	if max <= 0 {
		max = 60 * time.Second
	}
	if attempt < 0 {
		attempt = 0
	}

	d := base
	for i := 0; i < attempt && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)))
}

// Sleep, ctx'e saygı duyarak bekler.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrStop, koşunun tamamının durması gerektiğini bildirir.
// Captcha bu sınıfa girer: beklemek çözmez, kullanıcıya söylemek gerekir ve
// kapsam sınırı gereği captcha çözmeye çalışmıyoruz.
var ErrStop = errors.New("koşu durduruluyor")

// ErrExhausted, deneme bütçesi tükendiğinde döner. Item "kalıcı başarısız".
var ErrExhausted = errors.New("deneme bütçesi tükendi")

// Bu iki arayüz, hataların kendi yeniden deneme semantiğini taşımasını sağlar.
// Böylece net paketi site'ı veya dl'i import etmek zorunda kalmıyor; döngü
// yok ve sınıflandırma hatanın yanında duruyor.
type retryableError interface{ Retryable() bool }
type captchaError interface{ CaptchaRequired() bool }

// Policy, item başına yeniden deneme bütçesidir.
//
// İki ayrı üst sınır var ve ikisi de gerekli: MaxAttempts hızlı ve tekrarlayan
// hatalara karşı, MaxElapsed ise her denemesi uzun süren hatalara karşı. Yalnız
// biri olsa, yavaş bir hata 5 denemede saatler harcardı.
type Policy struct {
	MaxAttempts int
	MaxElapsed  time.Duration
	Backoff     Backoff
	// Logf nil olabilir.
	Logf func(format string, a ...any)
}

func (p Policy) logf(format string, a ...any) {
	if p.Logf != nil {
		p.Logf(format, a...)
	}
}

// Do, op'u bütçe tükenene veya kalıcı bir hata gelene kadar dener.
//
// Sınıflandırma sırası önemli: captcha her şeyden önce gelir, çünkü beklemek
// onu çözmez ve denemeye devam etmek durumu kötüleştirir.
func (p Policy) Do(ctx context.Context, op func(attempt int) error) error {
	attempts := p.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	deadline := time.Time{}
	if p.MaxElapsed > 0 {
		deadline = time.Now().Add(p.MaxElapsed)
	}

	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op(attempt)
		if err == nil {
			return nil
		}
		last = err

		// Context iptali yeniden denenmez; kullanıcı durmamızı istedi.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		var capt captchaError
		if errors.As(err, &capt) && capt.CaptchaRequired() {
			return fmt.Errorf("%w: captcha isteniyor, çözmeye çalışmıyoruz: %w", ErrStop, err)
		}

		var rt retryableError
		if !errors.As(err, &rt) || !rt.Retryable() {
			return err // kalıcı
		}

		if attempt == attempts-1 {
			break
		}
		d := p.Backoff.Delay(attempt)
		if !deadline.IsZero() && time.Now().Add(d).After(deadline) {
			p.logf("bütçe bitti (%s), beklenmiyor", p.MaxElapsed)
			break
		}
		p.logf("deneme %d/%d başarısız (%v), %s sonra tekrar", attempt+1, attempts, err, d.Round(time.Millisecond))
		if serr := Sleep(ctx, d); serr != nil {
			return serr
		}
	}
	return fmt.Errorf("%w (%d deneme): %w", ErrExhausted, attempts, last)
}
