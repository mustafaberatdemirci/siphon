package dl

// Adım 6'dan itibaren Download eşzamanlı çağrılıyor. Bu ortamda -race
// kullanılamıyor (cgo için C derleyicisi kurulu değil), bu yüzden yarış
// koşullarını gözlemlenebilir SONUÇLAR üzerinden kovalıyoruz: çakışan adlarla
// yüzlerce eşzamanlı indirme, beklenen dosya sayısı ve içerik doğruluğu.
//
// claim()'in kilidi kaldırılırsa bu testler dosya sayısı tutmadığı için düşer.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentDownloadsDistinctNames(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	const n = 64
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			it := testItem(srv.URL+"/veri.bin", fmt.Sprintf("dosya-%03d.bin", i))
			it.Index = i
			it.SHA256 = payloadSHA()
			_, errs[i] = d.Download(context.Background(), out, it)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("%d dosya olustu, %d bekleniyordu", len(entries), n)
	}
}

// Asil yaris koşulu burada: hepsi AYNI ada cozulüyor, yani claim() haritasina
// aynı anda yaziliyor. Kilit olmadan iki item ayni adi alir ve biri digerini
// ezer; sonuc n'den az dosya olur.
func TestConcurrentDownloadsCollidingNames(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	const n = 48
	var wg sync.WaitGroup
	errs := make([]error, n)
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			it := testItem(srv.URL+"/veri.bin", "ayni.bin")
			it.Index = i
			it.SourcePage = fmt.Sprintf("https://ornek.test/u/%d", i) // farkli item'lar
			it.SHA256 = payloadSHA()
			var res Result
			res, errs[i] = d.Download(context.Background(), out, it)
			paths[i] = res.Path
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}

	// Her item kendine ait bir yol almis olmali.
	seen := map[string]bool{}
	for i, p := range paths {
		if p == "" {
			t.Fatalf("item %d yol dondurmedi", i)
		}
		if seen[p] {
			t.Fatalf("iki item ayni yolu aldi: %s", p)
		}
		seen[p] = true
	}

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("%d dosya olustu, %d bekleniyordu; claim() yarisi var", len(entries), n)
	}

	// Icerik de dogru olmali: ezilme olsa hash kontrolu zaten dusurecekti ama
	// dosyalarin gercekten tam oldugunu ayrica dogruluyoruz.
	for _, e := range entries {
		b, rerr := os.ReadFile(filepath.Join(out, e.Name()))
		if rerr != nil {
			t.Fatalf("%s okunamadi: %v", e.Name(), rerr)
		}
		if !bytes.Equal(b, payload) {
			t.Fatalf("%s icerigi bozuk (%d bayt)", e.Name(), len(b))
		}
	}
}

// Aynı anda çalışan indirmeler ayrı klasörlere yazarken de karışmamalı.
func TestConcurrentDownloadsAcrossDirectories(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	const dirs, perDir = 8, 8
	var wg sync.WaitGroup
	for di := 0; di < dirs; di++ {
		for fi := 0; fi < perDir; fi++ {
			wg.Add(1)
			go func(di, fi int) {
				defer wg.Done()
				it := testItem(srv.URL+"/veri.bin", "ayni.bin")
				it.Dir = fmt.Sprintf("album-%d", di)
				it.Index = fi
				it.SourcePage = fmt.Sprintf("https://ornek.test/u/%d-%d", di, fi)
				it.SHA256 = payloadSHA()
				if _, err := d.Download(context.Background(), out, it); err != nil {
					t.Errorf("album-%d/%d: %v", di, fi, err)
				}
			}(di, fi)
		}
	}
	wg.Wait()

	for di := 0; di < dirs; di++ {
		dir := filepath.Join(out, fmt.Sprintf("album-%d", di))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if len(entries) != perDir {
			t.Errorf("%s icinde %d dosya, %d bekleniyordu", dir, len(entries), perDir)
		}
	}
}

// Eszamanli iptal: her indirme ya tamamlanmis ya da tutarli bir .part
// birakmis olmali. Yarim dosya ASLA nihai adla durmamali.
func TestConcurrentCancelLeavesNoFinalPartialFiles(t *testing.T) {
	release := make(chan struct{})
	srv := slowServer(t, 8000, release)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}

	ctx, cancel := context.WithCancel(context.Background())
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			it := testItem(srv.URL+"/veri.bin", fmt.Sprintf("kesilen-%02d.bin", i))
			it.Index = i
			it.SHA256 = payloadSHA()
			_, _ = d.Download(ctx, out, it)
		}(i)
	}
	cancel()
	wg.Wait()
	close(release)

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) == ".bin" {
			// Nihai adla duran bir dosya varsa tam olmak zorunda.
			b, rerr := os.ReadFile(filepath.Join(out, name))
			if rerr != nil {
				t.Fatalf("%s: %v", name, rerr)
			}
			if !bytes.Equal(b, payload) {
				t.Fatalf("%s nihai adla ama eksik (%d bayt)", name, len(b))
			}
		}
	}
}

// Downloader tek bir item'i iki kez indirmemeli: ikinci cagri "zaten var"
// dalina dusup ayni yolu dondurmeli.
func TestDownloadTwiceReturnsSamePath(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)

	it := testItem(srv.URL+"/veri.bin", "bir.bin")
	it.SHA256 = payloadSHA()

	d := &Downloader{Client: srv.Client()}
	r1, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatal(err)
	}
	// Ayni Downloader'da ikinci cagri claim() yuzunden yeni ad uretir; bu
	// dogru davranis (iki farkli item ayni ada cozulmus olabilir). Yeni bir
	// Downloader ise ayni adi hedefler ve "zaten var" dalina duser.
	d2 := &Downloader{Client: srv.Client()}
	r2, err := d2.Download(context.Background(), out, it)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Path != r2.Path {
		t.Fatalf("ikinci kosu farkli yol dondurdu:\n%s\n%s", r1.Path, r2.Path)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 1 {
		t.Fatalf("%d dosya var, 1 bekleniyordu", len(entries))
	}
}
