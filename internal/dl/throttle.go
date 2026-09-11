package dl

import (
	"context"
	"sync"
	"time"
)

// Throttle, tüm indirmelerin paylaştığı bayt/saniye sınırı. Jeton kovası:
// kova saniyede rate kadar dolar, en fazla bir saniyelik birikim tutar
// (patlama), her okuma okuduğu kadar jeton harcar; yoksa bekler.
//
// Neden global: kullanıcı "toplam 2 MB/s" ister, "dosya başına 2 MB/s"
// değil. Sınır 0 ise hiçbir şey beklemez ve maliyeti tek bir kilit almaktır.
//
// Hızı koşu sırasında değiştirmek güvenli: bir sonraki okuma yeni değeri
// görür.
type Throttle struct {
	mu     sync.Mutex
	rate   int64 // bayt/sn; 0 = sınırsız
	tokens float64
	last   time.Time
}

// SetRate, sınırı bayt/saniye olarak ayarlar; 0 kaldırır.
func (t *Throttle) SetRate(bytesPerSec int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if bytesPerSec < 0 {
		bytesPerSec = 0
	}
	t.rate = bytesPerSec
	// Yeni sınırda eski birikim taşmasın.
	if t.tokens > float64(bytesPerSec) {
		t.tokens = float64(bytesPerSec)
	}
	t.last = time.Now()
}

// Rate, geçerli sınırı döndürür.
func (t *Throttle) Rate() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rate
}

// Wait, n bayt için izin alınana kadar bekler. Sınır yoksa hemen döner.
func (t *Throttle) Wait(ctx context.Context, n int) error {
	if t == nil || n <= 0 {
		return nil
	}
	for {
		t.mu.Lock()
		if t.rate <= 0 {
			t.mu.Unlock()
			return nil
		}
		now := time.Now()
		if !t.last.IsZero() {
			t.tokens += now.Sub(t.last).Seconds() * float64(t.rate)
		}
		t.last = now
		if cap := float64(t.rate); t.tokens > cap {
			t.tokens = cap
		}
		if t.tokens >= float64(n) {
			t.tokens -= float64(n)
			t.mu.Unlock()
			return nil
		}
		deficit := float64(n) - t.tokens
		wait := time.Duration(deficit / float64(t.rate) * float64(time.Second))
		t.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// chunkFor, sınır varken okuma parçasını küçültür: 100 KB/s sınırda 256 KB
// okumak 2.5 saniyelik beklemeler ve takılmış görünen bir ilerleme çubuğu
// demek. Parça, saniyenin dörtte biri kadar veriye iniyor (en az 16 KB).
func (t *Throttle) chunkFor(bufLen int) int {
	if t == nil {
		return bufLen
	}
	t.mu.Lock()
	rate := t.rate
	t.mu.Unlock()
	if rate <= 0 {
		return bufLen
	}
	quarter := int(rate / 4)
	if quarter < 16<<10 {
		quarter = 16 << 10
	}
	if quarter < bufLen {
		return quarter
	}
	return bufLen
}
