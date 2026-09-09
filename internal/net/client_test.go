package net

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewClientHasNoOverallTimeout(t *testing.T) {
	c := NewClient()
	// Client.Timeout govde okumayi da kapsar; gigabaytlik bir indirmeyi
	// ortasindan keser. Bu alanin BOS kalmasi bilincli bir karar.
	if c.Timeout != 0 {
		t.Fatalf("Client.Timeout = %v, 0 olmaliydi (govde okumayi da kapsar)", c.Timeout)
	}
}

func TestHostLimiterCapsConcurrencyPerHost(t *testing.T) {
	const limit = 2
	const workers = 12
	l := NewHostLimiter(limit)

	var live, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Acquire(context.Background(), "ornek.test")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer release()
			n := live.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			live.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > limit {
		t.Fatalf("en yuksek eszamanlilik %d, sinir %d", got, limit)
	}
	if peak.Load() < 2 {
		t.Error("hic eszamanlilik olusmadi; test anlamsiz")
	}
}

// Sinir HOST basina: farkli hostlar birbirini bloklamamali. Aksi halde bir
// albumun CDN hostlari arasinda gereksiz daraltma olurdu.
func TestHostLimiterIsPerHost(t *testing.T) {
	l := NewHostLimiter(1)
	rel1, err := l.Acquire(context.Background(), "a.test")
	if err != nil {
		t.Fatal(err)
	}
	defer rel1()

	// Ayni host dolu: kisa ctx ile hemen zaman asimina dusmeli.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, "a.test"); err == nil {
		t.Error("ayni host icin ikinci yuva verildi, sinir 1 olmasina ragmen")
	}

	// Baska host serbest olmali.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	rel2, err := l.Acquire(ctx2, "b.test")
	if err != nil {
		t.Fatalf("farkli host bloklandi: %v", err)
	}
	rel2()
}

func TestHostLimiterReleaseIsIdempotent(t *testing.T) {
	l := NewHostLimiter(1)
	release, err := l.Acquire(context.Background(), "a.test")
	if err != nil {
		t.Fatal(err)
	}
	release()
	release() // iki kez cagirmak yuva sayisini bozmamali

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r2, err := l.Acquire(ctx, "a.test")
	if err != nil {
		t.Fatalf("yuva geri alinamadi: %v", err)
	}
	r2()
}

func TestHostOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://pixeldrain.com/api/file/x", "pixeldrain.com"},
		{"https://cdn.bunkr.ws:8443/a/b", "cdn.bunkr.ws:8443"},
		{"bozuk url", "?"},
		{"", "?"},
	}
	for _, c := range cases {
		if got := HostOf(c.in); got != c.want {
			t.Errorf("HostOf(%q) = %q, beklenen %q", c.in, got, c.want)
		}
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: 2 * time.Second}

	// Yarim jitter: bekleme [d/2, d) araliginda. Asla sifir olmamali, cunku
	// sifir bir denemeyi bosa harcar.
	for attempt := 0; attempt < 12; attempt++ {
		d := b.Delay(attempt)
		if d <= 0 {
			t.Fatalf("Delay(%d) = %v; sifir bir denemeyi bosa harcar", attempt, d)
		}
		if d > b.Max {
			t.Fatalf("Delay(%d) = %v, ust sinir %v", attempt, d, b.Max)
		}
	}

	// Buyume: ilk denemelerin ortalamasi sonrakilerden kucuk olmali.
	avg := func(attempt int) time.Duration {
		var total time.Duration
		for i := 0; i < 200; i++ {
			total += b.Delay(attempt)
		}
		return total / 200
	}
	if avg(0) >= avg(3) {
		t.Errorf("ustel buyume yok: attempt0=%v attempt3=%v", avg(0), avg(3))
	}
}

func TestBackoffJitterSpreadsValues(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Minute}
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[b.Delay(2)] = true
	}
	// Sabit gecikme, ayni anda limite carpan istekleri ayni anda geri getirip
	// suruyu yeniden olusturur. Jitter bunu dagitmak icin var.
	if len(seen) < 10 {
		t.Fatalf("%d farkli deger uretildi; jitter calismiyor", len(seen))
	}
}

func TestBackoffZeroValuesUseDefaults(t *testing.T) {
	var b Backoff
	d := b.Delay(0)
	if d <= 0 || d > time.Minute {
		t.Fatalf("varsayilanlar makul degil: %v", d)
	}
}

// --- Policy ---

type retryableTestErr struct{ n int }

func (e *retryableTestErr) Error() string   { return fmt.Sprintf("gecici %d", e.n) }
func (e *retryableTestErr) Retryable() bool { return true }

type captchaTestErr struct{}

func (captchaTestErr) Error() string         { return "captcha gerekli" }
func (captchaTestErr) Retryable() bool       { return false }
func (captchaTestErr) CaptchaRequired() bool { return true }

func fastPolicy(attempts int) Policy {
	return Policy{
		MaxAttempts: attempts,
		Backoff:     Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond},
	}
}

func TestPolicyRetriesThenExhausts(t *testing.T) {
	calls := 0
	err := fastPolicy(4).Do(context.Background(), func(attempt int) error {
		calls++
		return &retryableTestErr{n: attempt}
	})
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("ErrExhausted bekleniyordu: %v", err)
	}
	if calls != 4 {
		t.Fatalf("%d deneme yapildi, 4 bekleniyordu", calls)
	}
}

func TestPolicySucceedsAfterRetry(t *testing.T) {
	calls := 0
	err := fastPolicy(5).Do(context.Background(), func(int) error {
		calls++
		if calls < 3 {
			return &retryableTestErr{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("basarili olmaliydi: %v", err)
	}
	if calls != 3 {
		t.Fatalf("%d deneme, 3 bekleniyordu", calls)
	}
}

// Kalici hata tekrar denenmez: ayni cevabi getirir ve bant genisligi harcar.
func TestPolicyPermanentErrorIsNotRetried(t *testing.T) {
	permanent := errors.New("404 not found")
	calls := 0
	err := fastPolicy(5).Do(context.Background(), func(int) error {
		calls++
		return permanent
	})
	if !errors.Is(err, permanent) {
		t.Fatalf("kalici hata aynen donmeliydi: %v", err)
	}
	if calls != 1 {
		t.Fatalf("%d deneme yapildi, kalici hata 1 kez denenmeli", calls)
	}
}

// Captcha her seyden once gelir: beklemek cozmez ve denemeye devam etmek
// durumu kotulestirir. Kapsam siniri geregi captcha cozmuyoruz.
func TestPolicyCaptchaStopsImmediately(t *testing.T) {
	calls := 0
	err := fastPolicy(5).Do(context.Background(), func(int) error {
		calls++
		return captchaTestErr{}
	})
	if !errors.Is(err, ErrStop) {
		t.Fatalf("ErrStop bekleniyordu: %v", err)
	}
	if calls != 1 {
		t.Fatalf("%d deneme yapildi, captcha 1 kez denenmeli", calls)
	}
}

func TestPolicyContextCancelStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Policy{
		MaxAttempts: 10,
		Backoff:     Backoff{Base: 50 * time.Millisecond, Max: time.Second},
	}.Do(ctx, func(int) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return &retryableTestErr{}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context.Canceled bekleniyordu: %v", err)
	}
	if calls > 3 {
		t.Errorf("iptalden sonra %d deneme yapildi", calls)
	}
}

// MaxElapsed olmadan yavas bir hata 5 denemede saatler harcardi.
func TestPolicyMaxElapsedCutsWaiting(t *testing.T) {
	start := time.Now()
	calls := 0
	err := Policy{
		MaxAttempts: 20,
		MaxElapsed:  80 * time.Millisecond,
		Backoff:     Backoff{Base: 60 * time.Millisecond, Max: time.Second},
	}.Do(context.Background(), func(int) error {
		calls++
		return &retryableTestErr{}
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("ErrExhausted bekleniyordu: %v", err)
	}
	if calls >= 20 {
		t.Errorf("butce devreye girmedi: %d deneme", calls)
	}
	if elapsed > 2*time.Second {
		t.Errorf("butce asildi: %v", elapsed)
	}
}

func TestPolicyZeroAttemptsRunsOnce(t *testing.T) {
	calls := 0
	_ = Policy{}.Do(context.Background(), func(int) error {
		calls++
		return &retryableTestErr{}
	})
	if calls != 1 {
		t.Fatalf("%d deneme, sifir yapilandirmada 1 bekleniyordu", calls)
	}
}

func TestSleepRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("iptal edilmis ctx'te beklememeli: %v", err)
	}
}
