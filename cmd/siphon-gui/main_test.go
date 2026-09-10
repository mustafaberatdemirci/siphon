package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
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
	got := progressLine(it, 5<<20, -1, 0)
	if strings.Contains(got, "%") {
		t.Errorf("boyut bilinmiyorken yuzde basildi: %q", got)
	}
	if !strings.Contains(got, "5.0 MB") {
		t.Errorf("inen miktar yok: %q", got)
	}
	// Hiz bilinmiyorken "0 B/s" yazmak "durdu" demektir; hic yazilmamali.
	if strings.Contains(got, "/s") {
		t.Errorf("hiz bilinmiyorken hiz basildi: %q", got)
	}
}

func TestProgressLineWithKnownTotal(t *testing.T) {
	it := site.Item{Filename: "video.mp4"}
	got := progressLine(it, 50, 200, 0)
	if !strings.Contains(got, "%25") {
		t.Errorf("yuzde yanlis: %q", got)
	}
}

// Hiz ve kalan sure, bilindiginde satirda gorunmeli. Kullanicinin ilk
// sikayetlerinden biri buydu.
func TestProgressLineShowsSpeedAndETA(t *testing.T) {
	it := site.Item{Filename: "video.mp4"}
	// 100 MB'lik dosyanin 20 MB'i inmis, hiz 10 MB/s -> kalan ~8 sn.
	got := progressLine(it, 20<<20, 100<<20, 10<<20)
	if !strings.Contains(got, "MB/s") {
		t.Errorf("hiz yok: %q", got)
	}
	if !strings.Contains(got, "kalan") {
		t.Errorf("kalan sure yok: %q", got)
	}
	if !strings.Contains(got, "%20") {
		t.Errorf("yuzde yanlis: %q", got)
	}
}

// Boyut bilinmiyorken kalan sure UYDURULMAMALI.
func TestProgressLineNoETAWithoutTotal(t *testing.T) {
	it := site.Item{Filename: "video.mp4"}
	got := progressLine(it, 1<<20, -1, 5<<20)
	if strings.Contains(got, "kalan") {
		t.Errorf("boyut bilinmiyorken kalan sure basildi: %q", got)
	}
	if !strings.Contains(got, "/s") {
		t.Errorf("hiz basilmaliydi: %q", got)
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

// --- Hiz olcumu ---

func TestSpeedoNeedsTwoSamples(t *testing.T) {
	var sp speedo
	base := time.Now()
	// Ilk ornekten hiz cikarilamaz: gecen sure yok.
	if r := sp.update(0, base); r != 0 {
		t.Errorf("ilk ornek hiz uretti: %v", r)
	}
	r := sp.update(1<<20, base.Add(time.Second))
	if r <= 0 {
		t.Fatalf("ikinci ornekte hiz uretilmedi: %v", r)
	}
	// 1 MB / 1 sn
	if r < float64(1<<20)*0.9 || r > float64(1<<20)*1.1 {
		t.Errorf("hiz = %v, ~1 MB/s bekleniyordu", r)
	}
}

// Kumulatif sayac geriye giderse (indirme bastan basladi) negatif hiz
// uretilmemeli.
func TestSpeedoHandlesReset(t *testing.T) {
	var sp speedo
	base := time.Now()
	sp.update(10<<20, base)
	sp.update(20<<20, base.Add(time.Second))
	r := sp.update(0, base.Add(2*time.Second))
	if r < 0 {
		t.Fatalf("negatif hiz: %v", r)
	}
}

func TestSpeedoIgnoresZeroInterval(t *testing.T) {
	var sp speedo
	now := time.Now()
	sp.update(0, now)
	sp.update(1<<20, now.Add(time.Second))
	before := sp.rate()
	// Ayni ana iki olcum: sifira bolme olmamali.
	got := sp.update(2<<20, now.Add(time.Second))
	if got != before {
		t.Errorf("sifir aralikta hiz degisti: %v -> %v", before, got)
	}
}

func TestHumanRateEmptyWhenUnknown(t *testing.T) {
	// "0 B/s" yazmak kullaniciya "durdu" der; olcum yoksa hic yazma.
	if got := humanRate(0); got != "" {
		t.Errorf("humanRate(0) = %q, bos bekleniyordu", got)
	}
	if got := humanRate(-5); got != "" {
		t.Errorf("humanRate(-5) = %q, bos bekleniyordu", got)
	}
	if got := humanRate(1 << 20); got != "1.0 MB/s" {
		t.Errorf("humanRate = %q", got)
	}
}

func TestHumanETA(t *testing.T) {
	if got := humanETA(0, 100); got != "" {
		t.Errorf("kalan sifirken ETA basildi: %q", got)
	}
	if got := humanETA(100, 0); got != "" {
		t.Errorf("hiz bilinmiyorken ETA basildi: %q", got)
	}
	if got := humanETA(30, 1); !strings.Contains(got, "sn") {
		t.Errorf("saniye bekleniyordu: %q", got)
	}
	if got := humanETA(300, 1); !strings.Contains(got, "dk") {
		t.Errorf("dakika bekleniyordu: %q", got)
	}
	if got := humanETA(7200, 1); !strings.Contains(got, "sa") {
		t.Errorf("saat bekleniyordu: %q", got)
	}
	// Bir gunden uzun tahmin anlamsiz; uydurma sayi verme.
	if got := humanETA(1<<40, 1); got != "" {
		t.Errorf("cok uzun tahmin basildi: %q", got)
	}
}

// --- Canli toplam sayaci ---

// ASIL HATA BUYDU: toplam, URLResolved'dan sayiliyordu ve o olay is bitince
// tetikleniyor; ilerleme cubugu kosu boyunca sifirda kaliyordu. Artik
// ItemQueued'dan sayiliyor.
func TestTotalCountsFromQueuedNotFromURLResolved(t *testing.T) {
	u := newUI(nil)
	ev := u.events()
	if ev.ItemQueued == nil {
		t.Fatal("ItemQueued baglanmamis; toplam yine gec sayilir")
	}

	for i := 0; i < 5; i++ {
		ev.ItemQueued(site.Item{
			SourcePage: fmt.Sprintf("https://s.test/f/%d", i),
			Filename:   fmt.Sprintf("d%d.bin", i),
		})
	}
	u.mu.Lock()
	total := u.total
	u.mu.Unlock()
	if total != 5 {
		t.Fatalf("toplam = %d, 5 bekleniyordu", total)
	}

	// Iki dosya bitince cubuk %40 olmali; is bitmeden ilerleme gorunmeli.
	u.bump()
	u.bump()
	v, _ := u.progress.Get()
	if v < 0.39 || v > 0.41 {
		t.Fatalf("ilerleme = %v, 0.4 bekleniyordu", v)
	}
}

// trackBytes kumulatif deger aliyor; toplam bayt farklardan hesaplanmali,
// yoksa her bildirimde toplam sisirilir.
func TestTrackBytesAccumulatesDeltas(t *testing.T) {
	u := newUI(nil)
	it := site.Item{SourcePage: "https://s.test/f/1", Filename: "a.bin"}

	u.trackBytes(it, 1000)
	u.trackBytes(it, 3000)
	u.trackBytes(it, 5000)

	u.mu.Lock()
	total := u.totalBytes
	u.mu.Unlock()
	if total != 5000 {
		t.Fatalf("toplam bayt = %d, 5000 bekleniyordu (kumulatif deger tekrar tekrar toplanmis olabilir)", total)
	}
}

func TestTrackBytesAcrossItems(t *testing.T) {
	u := newUI(nil)
	a := site.Item{SourcePage: "https://s.test/f/a", Filename: "a.bin"}
	b := site.Item{SourcePage: "https://s.test/f/b", Filename: "b.bin"}

	u.trackBytes(a, 1000)
	u.trackBytes(b, 2000)
	u.trackBytes(a, 4000)

	u.mu.Lock()
	total := u.totalBytes
	u.mu.Unlock()
	if total != 6000 {
		t.Fatalf("toplam bayt = %d, 6000 bekleniyordu", total)
	}
}

// Indirme bastan baslarsa (kumulatif deger geriye giderse) toplam sisip
// bozulmamali.
func TestTrackBytesHandlesRestart(t *testing.T) {
	u := newUI(nil)
	it := site.Item{SourcePage: "https://s.test/f/1", Filename: "a.bin"}

	u.trackBytes(it, 5000)
	u.trackBytes(it, 100) // sunucu 200 dondu, .part sifirlandi
	u.trackBytes(it, 900)

	u.mu.Lock()
	total := u.totalBytes
	u.mu.Unlock()
	if total != 900 {
		t.Fatalf("toplam bayt = %d, 900 bekleniyordu (yeniden baslatma sonrasi)", total)
	}
}

func TestTrackBytesIsConcurrentSafe(t *testing.T) {
	u := newUI(nil)
	const items, steps = 20, 20
	var wg sync.WaitGroup
	for i := 0; i < items; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			it := site.Item{
				SourcePage: fmt.Sprintf("https://s.test/f/%d", i),
				Filename:   fmt.Sprintf("d%d.bin", i),
			}
			for j := 1; j <= steps; j++ {
				u.trackBytes(it, int64(j*100))
			}
		}(i)
	}
	wg.Wait()

	u.mu.Lock()
	total := u.totalBytes
	u.mu.Unlock()
	want := int64(items * steps * 100)
	if total != want {
		t.Fatalf("toplam bayt = %d, %d bekleniyordu", total, want)
	}
}

// Olay zinciri: ItemQueued satiri acmali, Progress ayni satiri yuzde ve hizla
// GUNCELLEMELI (yeni satir acmamali), ItemDone da ayni satiri bitirmeli.
// Kullanicinin "anlik gozukmuyor" sikayeti tam olarak bu zincirdeydi.
func TestEventChainUpdatesSameRow(t *testing.T) {
	u := newUI(nil)
	ev := u.events()
	it := site.Item{
		SourcePage: "https://s.test/f/1",
		Filename:   "video.mp4",
		Size:       200,
	}

	ev.ItemQueued(it)
	if n := u.items.Length(); n != 1 {
		t.Fatalf("ItemQueued sonrasi %d satir, 1 bekleniyordu", n)
	}
	first, _ := u.items.GetValue(0)
	if !strings.Contains(first, "sırada") {
		t.Errorf("kuyruk satiri yanlis: %q", first)
	}

	// Iki olcum: ilkinden hiz cikmaz, ikincisinden cikar.
	ev.Progress(it, 50, 200)
	ev.Progress(it, 150, 200)

	if n := u.items.Length(); n != 1 {
		t.Fatalf("Progress yeni satir acti: %d satir", n)
	}
	line, _ := u.items.GetValue(0)
	if !strings.Contains(line, "%75") {
		t.Errorf("yuzde guncellenmedi: %q", line)
	}

	ev.ItemDone(it, dl.Result{Path: `C:\out\video.mp4`, Size: 200})
	if n := u.items.Length(); n != 1 {
		t.Fatalf("ItemDone yeni satir acti: %d satir", n)
	}
	done, _ := u.items.GetValue(0)
	if !strings.HasPrefix(done, "✓") {
		t.Errorf("bitis satiri yanlis: %q", done)
	}
}

// Durum satiri, indirme surerken toplam ve hiz gostermeli; is bitmeden.
func TestStatusShowsLiveTotals(t *testing.T) {
	u := newUI(nil)
	ev := u.events()
	a := site.Item{SourcePage: "https://s.test/f/a", Filename: "a.bin"}
	b := site.Item{SourcePage: "https://s.test/f/b", Filename: "b.bin"}

	ev.ItemQueued(a)
	ev.ItemQueued(b)
	ev.Progress(a, 1<<20, 10<<20)

	got, _ := u.status.Get()
	if !strings.Contains(got, "0/2 dosya") {
		t.Errorf("canli toplam yok: %q", got)
	}
	if !strings.Contains(got, "indirildi") {
		t.Errorf("inen miktar yok: %q", got)
	}
}

// --- Klasor yolu normalizasyonu ---

// OLCULDU: Fyne'in klasor seciciden dondurdugu yol EGIK CIZGILI ("E:/x").
// Go'nun dosya cagrilari bunu kabul ettigi icin indirme dogru yere iniyordu,
// ama explorer.exe kabul etmiyor ve sessizce Belgeler klasorunu aciyordu.
// Kullanicinin bildirdigi hata tam olarak buydu.
func TestNormalizeDirConvertsPickerPathForExplorer(t *testing.T) {
	const want = `E:\x`
	got := normalizeDir("E:/x")
	if got != want {
		t.Fatalf("normalizeDir(%q) = %q, %q bekleniyordu: egik cizgili yol explorer'da Belgeler'i acar", "E:/x", got, want)
	}
}

func TestNormalizeDirStripsTrailingSeparator(t *testing.T) {
	// Sondaki ters cizgi komut satirinda kapanis tirnagini kacirir ve
	// explorer yolu yine tanimaz.
	const want = `E:\x`
	if got := normalizeDir(`E:\x\`); got != want {
		t.Errorf("normalizeDir = %q, %q bekleniyordu", got, want)
	}
	if got := normalizeDir("E:/x/"); got != want {
		t.Errorf("normalizeDir = %q, %q bekleniyordu", got, want)
	}
}

func TestNormalizeDirStripsQuotes(t *testing.T) {
	// Windows'un "Yol olarak kopyala" komutu yolu tirnak icinde veriyor.
	if got := normalizeDir(`"E:\x"`); got != `E:\x` {
		t.Errorf("normalizeDir = %q, %q bekleniyordu", got, `E:\x`)
	}
	if got := normalizeDir(`  "E:\alt klasor"  `); got != `E:\alt klasor` {
		t.Errorf("normalizeDir bosluklu yolu bozdu: %q", got)
	}
}

func TestNormalizeDirEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", `""`} {
		if got := normalizeDir(in); got != "" {
			t.Errorf("normalizeDir(%q) = %q, bos bekleniyordu", in, got)
		}
	}
}

// Normalizasyon yolu BOZMAMALI: gecerli bir yol ayni kalmali.
func TestNormalizeDirKeepsValidPath(t *testing.T) {
	for _, in := range []string{`E:\x`, `C:\Users\musta\Downloads\siphon`, `\sunucu\pay\klasor`} {
		if got := normalizeDir(in); got != in {
			t.Errorf("normalizeDir(%q) = %q; gecerli yol degistirilmemeliydi", in, got)
		}
	}
}

// Kullanicinin gercek durumu: klasor secicide E: surucusunun kokunu sectiginde
// Path() "E:/" donuyor. Ciplak "E:" ise SURUCUYE GORELI bir yol ("E: nin
// gecerli dizini"); klasor kutusuna yazan kimse bunu kastetmez.
func TestNormalizeDirDriveRoot(t *testing.T) {
	if got := normalizeDir("E:/"); got != `E:\` {
		t.Errorf("normalizeDir(\"E:/\") = %q, %q bekleniyordu", got, `E:\`)
	}
	if got := normalizeDir("E:"); got != `E:\` {
		t.Errorf("normalizeDir(\"E:\") = %q, %q bekleniyordu; %q surucuye goreli yoldur", got, `E:\`, "E:.")
	}
	if got := normalizeDir(`e:`); got != `e:\` {
		t.Errorf("normalizeDir kucuk harf surucu = %q", got)
	}
}

// Sadece listele modunda run yalnizca ItemResolved gonderiyor. Arayuz
// toplami ItemQueued'dan saydigi icin bu modda durum satiri bastan sona
// "cozumleniyor..." kaliyordu.
func TestListOnlyModeReportsFoundCount(t *testing.T) {
	u := newUI(nil)
	ev := u.events()
	for i := 0; i < 3; i++ {
		ev.ItemResolved(site.Item{
			SourcePage: fmt.Sprintf("https://s.test/f/%d", i),
			URL:        fmt.Sprintf("https://cdn.test/%d.bin", i),
			Filename:   fmt.Sprintf("d%d.bin", i),
		})
	}
	got, _ := u.status.Get()
	if !strings.Contains(got, "3") {
		t.Errorf("listeleme sirasinda bulunan dosya sayisi gorunmuyor: %q", got)
	}
	if strings.Contains(got, "çözümleniyor") {
		t.Errorf("durum satiri hala 'cozumleniyor' diyor: %q", got)
	}
}
