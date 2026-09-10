package main

import (
	"fmt"
	"sync"
	"time"
)

// speedo, bayt/saniye tahmini üretir.
//
// Neden düz bir bölme değil: 250 ms'lik pencerelerde anlık ölçüm çok zıplıyor
// (ağ dalgalanıyor, disk tamponu boşalıyor) ve kullanıcı sürekli değişen bir
// sayı görüyor. Koşu başından beri düz ortalama ise yavaşlamaları gösteremiyor;
// hız yarıya düşse bile ortalama dakikalarca yüksek kalıyor.
//
// Üstel hareketli ortalama ikisinin arasında: son ölçüme ağırlık verir ama tek
// bir sıçramayla savrulmaz.
type speedo struct {
	mu        sync.Mutex
	last      time.Time
	lastBytes int64
	ema       float64
	started   bool
}

// speedAlpha, son ölçümün ağırlığı. 0.3 pratikte okunabilir bir denge:
// yavaşlamayı birkaç saniyede gösteriyor, ama sayıyı titretmiyor.
const speedAlpha = 0.3

// update, yeni bir kümülatif bayt değeri işler ve güncel hızı döndürür.
func (s *speedo) update(bytes int64, now time.Time) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		// İlk örnekten hız çıkarılamaz: geçen süre yok.
		s.started, s.last, s.lastBytes = true, now, bytes
		return 0
	}

	dt := now.Sub(s.last).Seconds()
	if dt <= 0 {
		return s.ema
	}
	db := bytes - s.lastBytes
	if db < 0 {
		// Kümülatif sayaç geriye gitti: indirme baştan başlamış (sunucu 200
		// döndü, .part sıfırlandı). Negatif hız göstermek yerine sıfırdan devam.
		db = 0
	}
	inst := float64(db) / dt
	if s.ema == 0 {
		s.ema = inst
	} else {
		s.ema = speedAlpha*inst + (1-speedAlpha)*s.ema
	}
	s.last, s.lastBytes = now, bytes
	return s.ema
}

// rate, son hesaplanan hızı döndürür.
func (s *speedo) rate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ema
}

// humanRate, hızı okunur biçime çevirir. Hız bilinmiyorsa boş string döner:
// "0 B/s" yazmak kullanıcıya "durdu" der, oysa henüz ölçüm yok.
func humanRate(bps float64) string {
	if bps <= 0 {
		return ""
	}
	return humanBytes(int64(bps)) + "/s"
}

// humanETA, kalan süreyi okunur biçime çevirir.
// Hız veya kalan bilinmiyorsa boş döner; uydurma tahmin göstermiyoruz.
func humanETA(remaining int64, bps float64) string {
	if remaining <= 0 || bps <= 0 {
		return ""
	}
	secs := float64(remaining) / bps
	if secs > 24*3600 {
		// Bir günden uzun tahminler pratikte anlamsız; sayı vermek yanıltıcı.
		return ""
	}
	d := time.Duration(secs) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d sn", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d dk", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d sa %d dk", int(d.Hours()), int(d.Minutes())%60)
	}
}
