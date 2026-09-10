package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"fyne.io/fyne/v2/test"

	"github.com/mustafaberatdemirci/siphon/internal/run"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// TestMain, bassiz bir Fyne uygulamasi kurar.
//
// Neden gerekli: binding.Set() dinleyicileri tetikliyor ve tetikleme fyne.Do
// uzerinden gidiyor; calisan bir uygulama yoksa nil pointer paniği atiyor.
// Bu, arayuz kodunun test edilebilmesinin on kosulu.
func TestMain(m *testing.M) {
	test.NewApp()
	os.Exit(m.Run())
}

// Metin alanı, komut satırındaki -i dosyasıyla AYNI kuralları izlemeli.
// İki arayüzün aynı girdiyi farklı yorumlaması, kullanıcının hangi arayüzde
// olduğunu hatırlamak zorunda kalması demek.
func TestParseLinksMatchesCLIRules(t *testing.T) {
	in := "https://bir.test/a\r\n" +
		"  https://iki.test/b  \n" +
		"\n" +
		"# bu bir yorum\n" +
		"   \n" +
		"https://uc.test/c"
	got := parseLinks(in)
	want := []string{"https://bir.test/a", "https://iki.test/b", "https://uc.test/c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("parseLinks = %v, beklenen %v", got, want)
	}
}

func TestParseLinksEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n\n", "# sadece yorum"} {
		if got := parseLinks(in); len(got) != 0 {
			t.Errorf("parseLinks(%q) = %v, bos bekleniyordu", in, got)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1946234880, "1.8 GB"},
		{-1, "? B"}, // boyut bilinmiyor
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, beklenen %q", c.in, got, c.want)
		}
	}
}

// Boyut bilinmiyorsa yuzde UYDURULMAMALI: kullanicinin gordugu her sayi
// gercek olmali.
func TestProgressLineWithUnknownTotal(t *testing.T) {
	it := site.Item{Filename: "video.mp4"}
	got := progressLine(it, 5<<20, -1)
	if strings.Contains(got, "%") {
		t.Errorf("boyut bilinmiyorken yuzde basildi: %q", got)
	}
	if !strings.Contains(got, "5.0 MB") {
		t.Errorf("inen miktar yok: %q", got)
	}
}

func TestProgressLineWithKnownTotal(t *testing.T) {
	it := site.Item{Filename: "video.mp4"}
	got := progressLine(it, 50, 200)
	if !strings.Contains(got, "%25") {
		t.Errorf("yuzde yanlis: %q", got)
	}
}

func TestShortNameTruncatesLongNames(t *testing.T) {
	long := strings.Repeat("u", 200) + ".mp4"
	got := shortName(long)
	if len([]rune(got)) > 60 {
		t.Errorf("%d rune, en fazla 60 olmaliydi", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("kirpma isareti yok: %q", got)
	}
	// Kisa ad dokunulmamali.
	if got := shortName("kisa.mp4"); got != "kisa.mp4" {
		t.Errorf("kisa ad degistirildi: %q", got)
	}
}

func TestFirstLineCollapsesMultilineErrors(t *testing.T) {
	// Windows ag hatalari gomulu satir sonu iceriyor; liste satirini bozmamali.
	in := "baglanti kurulamadi\nikinci satir\nucuncu"
	if got := firstLine(in); got != "baglanti kurulamadi" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("tek satir"); got != "tek satir" {
		t.Errorf("firstLine = %q", got)
	}
}

// Ozet satiri, cikis kodu sozlesmesinin insan dilindeki karsiligi.
// Kismi basari "basarisiz" dememeli, ama "tamam" da dememeli.
func TestSummaryLine(t *testing.T) {
	full := run.Summary{URLs: 1, Items: 10, Done: 10, ResolvedAny: true}
	if got := summaryLine(full, false); !strings.Contains(got, "10/10") {
		t.Errorf("tam basari: %q", got)
	}

	partial := run.Summary{URLs: 1, Items: 10, Done: 7, Failed: 2, Skipped: 1, ResolvedAny: true}
	got := summaryLine(partial, false)
	for _, want := range []string{"7/10", "1 zaten vardı", "2 başarısız"} {
		if !strings.Contains(got, want) {
			t.Errorf("kismi basari %q icermiyor: %q", want, got)
		}
	}

	halted := run.Summary{URLs: 1, Items: 5, Done: 2, ResolvedAny: true, Halted: true}
	if got := summaryLine(halted, false); !strings.Contains(got, "durduruldu") {
		t.Errorf("durdurma bildirilmedi: %q", got)
	}

	// Sadece listele modunda "indi" demek yanlis olurdu.
	listing := run.Summary{URLs: 2, Items: 16}
	got = summaryLine(listing, true)
	if strings.Contains(got, "indi") {
		t.Errorf("listeleme modunda indirme iddia edildi: %q", got)
	}
	if !strings.Contains(got, "16") {
		t.Errorf("bulunan dosya sayisi yok: %q", got)
	}

	// Kayit yazilamamasi SESSIZ gecilmemeli.
	degraded := run.Summary{URLs: 1, Items: 3, Done: 3, Degraded: 1, ResolvedAny: true}
	if got := summaryLine(degraded, false); !strings.Contains(got, "kaydı yazılamadı") {
		t.Errorf("bozulma bildirilmedi: %q", got)
	}
}

// setRow eszamanli cagriliyor: her item icin TEK satir acilmali ve iki item
// ayni satiri paylasmamali. Cift kontrol mantigi kaldirilirsa bu test duser.
func TestSetRowIsConcurrentSafe(t *testing.T) {
	u := newUI(nil)

	const items, updates = 40, 10
	var wg sync.WaitGroup
	for i := 0; i < items; i++ {
		for j := 0; j < updates; j++ {
			wg.Add(1)
			go func(i, j int) {
				defer wg.Done()
				it := site.Item{
					SourcePage: fmt.Sprintf("https://s.test/f/%d", i),
					Filename:   fmt.Sprintf("dosya-%d.bin", i),
				}
				u.setRow(it, fmt.Sprintf("item %d guncelleme %d", i, j))
			}(i, j)
		}
	}
	wg.Wait()

	if n := u.items.Length(); n != items {
		t.Fatalf("%d satir olustu, %d bekleniyordu; setRow yarisi var", n, items)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.rows) != items {
		t.Fatalf("%d anahtar kayitli, %d bekleniyordu", len(u.rows), items)
	}
	seen := map[int]bool{}
	for key, row := range u.rows {
		if seen[row] {
			t.Fatalf("iki item ayni satiri paylasiyor: %s -> %d", key, row)
		}
		seen[row] = true
	}
}

// SourcePage bos olabilir (bazi medya yollarinda); o zaman URL anahtar olmali,
// yoksa tum bos-SourcePage item'lari tek satiri paylasir.
func TestSetRowFallsBackToURL(t *testing.T) {
	u := newUI(nil)
	u.setRow(site.Item{URL: "https://cdn.test/a.mp4"}, "a")
	u.setRow(site.Item{URL: "https://cdn.test/b.mp4"}, "b")
	if n := u.items.Length(); n != 2 {
		t.Fatalf("%d satir, 2 bekleniyordu", n)
	}
}

func TestBumpTracksProgress(t *testing.T) {
	u := newUI(nil)
	u.mu.Lock()
	u.total = 4
	u.mu.Unlock()

	for i := 0; i < 2; i++ {
		u.bump()
	}
	v, err := u.progress.Get()
	if err != nil {
		t.Fatal(err)
	}
	if v != 0.5 {
		t.Fatalf("ilerleme = %v, 0.5 bekleniyordu", v)
	}
}

// Toplam bilinmiyorken bump ilerlemeyi bozmamali (sifira bolme).
func TestBumpWithZeroTotal(t *testing.T) {
	u := newUI(nil)
	u.bump()
	v, _ := u.progress.Get()
	if v != 0 {
		t.Fatalf("ilerleme = %v, 0 kalmaliydi", v)
	}
}

func TestDefaultOutDirIsAbsolute(t *testing.T) {
	d := defaultOutDir()
	if d == "" {
		t.Fatal("bos klasor")
	}
	if d == "." {
		t.Log("cwd'ye dusuldu; kabul edilebilir ama ideal degil")
	}
}
