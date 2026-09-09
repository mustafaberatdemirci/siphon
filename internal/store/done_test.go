package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestKeyDoesNotDependOnURL(t *testing.T) {
	// Anahtar imzasinda URL YOK. bunkr'in imzali adresi her kosuda degisiyor;
	// URL ile anahtarlamak idempotent yeniden baslatmayi her seferinde bozardi.
	// Bu test imzanin kendisini sabitliyor: Key'e URL eklenirse derlenmez.
	a := Key("albüm", "https://bunkr.ws/f/slug", "dosya.mp4")
	b := Key("albüm", "https://bunkr.ws/f/slug", "dosya.mp4")
	if a != b {
		t.Fatalf("ayni item farkli anahtar uretti: %q vs %q", a, b)
	}
	if a == "" {
		t.Fatal("bos anahtar")
	}
}

// Duz birlestirme ("a"+"bc" ile "ab"+"c") iki farkli item'i ayni anahtara
// dusurebilir. Dosya adlari her karakteri icerebildigi icin guvenli bir
// ayirici yok; uzunluk oneki bu yuzden var.
func TestKeyIsUnambiguous(t *testing.T) {
	pairs := [][2][3]string{
		{{"a", "bc", "x"}, {"ab", "c", "x"}},
		{{"", "ab", "c"}, {"a", "b", "c"}},
		{{"a|b", "c", "d"}, {"a", "b|c", "d"}},
		{{"x", "", ""}, {"", "x", ""}},
	}
	for _, p := range pairs {
		k1 := Key(p[0][0], p[0][1], p[0][2])
		k2 := Key(p[1][0], p[1][1], p[1][2])
		if k1 == k2 {
			t.Errorf("cakisma: %v ve %v ayni anahtar (%s)", p[0], p[1], k1)
		}
	}
}

func TestOpenPlacesLedgerUnderOutRoot(t *testing.T) {
	// Kayit CIKTI KOKUNUN altinda olmali. cwd'de veya exe yaninda olsaydi,
	// farkli bir -out ile ikinci kosu "hepsi indi" deyip hicbir sey indirmeden
	// 0 donerdi.
	out := filepath.Join(t.TempDir(), "cikti", "alt")
	l, err := Open(out)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	want := filepath.Join(out, FileName)
	if l.Path() != want {
		t.Fatalf("kayit yolu = %q, beklenen %q", l.Path(), want)
	}
	if _, serr := os.Stat(want); serr != nil {
		t.Fatalf("kayit dosyasi olusmadi: %v", serr)
	}
}

func TestAddAndLookup(t *testing.T) {
	out := t.TempDir()
	l, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	e := Entry{
		SourcePage: "https://bunkr.ws/f/slug",
		Dir:        "Albüm 2026",
		Filename:   "bir.mp4",
		Path:       filepath.Join("Albüm 2026", "bir.mp4"),
		Size:       1234,
		SHA256:     "deadbeef",
	}
	if err := l.Add(e); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if l.Len() != 1 {
		t.Fatalf("Len = %d, 1 bekleniyordu", l.Len())
	}

	got, ok := l.Lookup(e.Dir, e.SourcePage, e.Filename)
	if !ok {
		t.Fatal("eklenen item bulunamadi")
	}
	if got.Path != e.Path || got.Size != 1234 || got.SHA256 != "deadbeef" {
		t.Errorf("kayit bozuk: %+v", got)
	}
	if got.TS == "" {
		t.Error("zaman damgasi doldurulmadi")
	}

	if _, ok := l.Lookup("baska", e.SourcePage, e.Filename); ok {
		t.Error("farkli dir ayni anahtara dustu")
	}
}

// Asil kriter: kayit KOSULAR ARASINDA yasiyor.
func TestLedgerSurvivesReopen(t *testing.T) {
	out := t.TempDir()
	l, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Add(Entry{
			SourcePage: fmt.Sprintf("https://s/%d", i),
			Dir:        "d",
			Filename:   fmt.Sprintf("f%d.bin", i),
			Path:       fmt.Sprintf("d/f%d.bin", i),
			Size:       int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l2, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.Len() != 3 {
		t.Fatalf("yeniden acilista Len = %d, 3 bekleniyordu", l2.Len())
	}
	if _, ok := l2.Lookup("d", "https://s/1", "f1.bin"); !ok {
		t.Error("onceki kosunun kaydi okunamadi")
	}
	// Ekleme append-only: eski satirlar korunmus olmali.
	data, _ := os.ReadFile(l2.Path())
	if n := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; n != 3 {
		t.Errorf("%d satir var, 3 bekleniyordu", n)
	}
}

// Cokme aninda yarim yazilmis son satir BEKLENEN durum. Kaydi bozuk sayip
// kosuyu dusurmek, kurtarilabilir bir durumu olumcul yapmak olurdu.
// Ama sessiz de gecilmiyor: sayiliyor.
func TestCorruptLinesAreSkippedNotFatal(t *testing.T) {
	out := t.TempDir()
	path := filepath.Join(out, FileName)
	good, _ := json.Marshal(Entry{
		SourcePage: "https://s/1", Dir: "d", Filename: "iyi.bin", Path: "d/iyi.bin",
	})
	content := string(good) + "\n" +
		"{bu gecerli json degil\n" +
		"\n" +
		`{"source_page":"https://s/2","dir":"d"}` + "\n" + // filename yok
		`{"source_page":"https://s/3","dir":"d","filename":"yarim.bin"` // kapanmamis
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Open(out)
	if err != nil {
		t.Fatalf("bozuk satir kosuyu dusurmemeliydi: %v", err)
	}
	defer l.Close()

	if l.Len() != 1 {
		t.Fatalf("Len = %d, yalnizca iyi satir okunmaliydi", l.Len())
	}
	if _, ok := l.Lookup("d", "https://s/1", "iyi.bin"); !ok {
		t.Error("iyi satir kaybedildi")
	}
	// Bos satir sayilmiyor; diger uc satir bozuk.
	if l.Skipped() != 3 {
		t.Errorf("Skipped = %d, 3 bekleniyordu", l.Skipped())
	}
}

// Uzun dosya adlari ve yollar varsayilan 64 KB tarayici sinirini asabilir.
func TestLongLinesAreRead(t *testing.T) {
	out := t.TempDir()
	long := strings.Repeat("u", 200000)
	e := Entry{SourcePage: "https://s/1", Dir: long, Filename: "f.bin", Path: "f.bin"}
	data, _ := json.Marshal(e)
	if err := os.WriteFile(filepath.Join(out, FileName), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Len() != 1 {
		t.Fatalf("uzun satir okunamadi (Len=%d, Skipped=%d)", l.Len(), l.Skipped())
	}
}

func TestAddRequiresFilename(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Add(Entry{SourcePage: "https://s", Dir: "d"}); err == nil {
		t.Fatal("dosya adi olmadan kayit kabul edildi")
	}
}

func TestAddAfterCloseFails(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Add(Entry{Filename: "f.bin"}); err == nil {
		t.Fatal("kapali kayda yazildi")
	}
	// Close idempotent olmali.
	if err := l.Close(); err != nil {
		t.Errorf("ikinci Close hata verdi: %v", err)
	}
}

// Indirmeler esanzamanli; kayit da esanzamanli yazilacak.
func TestConcurrentAdd(t *testing.T) {
	out := t.TempDir()
	l, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}

	const n = 40
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = l.Add(Entry{
				SourcePage: fmt.Sprintf("https://s/%d", i),
				Dir:        "d",
				Filename:   fmt.Sprintf("f%d.bin", i),
				Path:       fmt.Sprintf("d/f%d.bin", i),
			})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("item %d: %v", i, e)
		}
	}
	if l.Len() != n {
		t.Fatalf("Len = %d, %d bekleniyordu", l.Len(), n)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Satirlar birbirine karismamis olmali: her satir gecerli JSON.
	l2, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.Skipped() != 0 {
		t.Errorf("%d satir bozulmus; esanzamanli yazim satirlari karistiriyor", l2.Skipped())
	}
	if l2.Len() != n {
		t.Errorf("yeniden acilista Len = %d, %d bekleniyordu", l2.Len(), n)
	}
}

func TestOpenFailsOnUnusableRoot(t *testing.T) {
	// Var olan bir DOSYAyi cikti koku olarak vermek hata vermeli; sessizce
	// kayitsiz devam etmek idempotence'i sessizce kapatmak olurdu.
	f := filepath.Join(t.TempDir(), "dosya")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f); err == nil {
		t.Fatal("dosya yolunda Open basarili oldu")
	}
}
