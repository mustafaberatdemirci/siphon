package dl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComponentForbiddenCharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{`a<b>c:d"e|f?g*h`, "a-b-c-d-e-f-g-h"},
		{`yol/ayirici`, "yol-ayirici"},
		{`ters\ayirici`, "ters-ayirici"},
		// Kontrol karakterleri tamamen atilir, "-" koymak gurultu uretir.
		{"sekme\tve\nsatir", "sekmevesatir"},
		{"bel\x7fkarakteri", "belkarakteri"},
		// Normal karakterler dokunulmaz; Turkce ve bosluk korunur.
		{"Tatil 2026 - Özgür & Aslı.mp4", "Tatil 2026 - Özgür & Aslı.mp4"},
	}
	for _, c := range cases {
		if got := Component(c.in); got != c.want {
			t.Errorf("Component(%q) = %q, beklenen %q", c.in, got, c.want)
		}
	}
}

// Windows sondaki nokta ve bosluklari SESSIZCE atar. Biz atmazsak disk uzerinde
// olusan ad, bizim sandigimiz addan farkli olur ve "zaten var" kontrolu ile
// rename beklenmedik davranir.
func TestComponentTrailingDotsAndSpaces(t *testing.T) {
	cases := []struct{ in, want string }{
		{"dosya.", "dosya"},
		{"dosya...", "dosya"},
		{"dosya ", "dosya"},
		{"dosya. . ", "dosya"},
		{"  bastaki bosluk", "bastaki bosluk"},
		{"dosya.txt", "dosya.txt"},
	}
	for _, c := range cases {
		if got := Component(c.in); got != c.want {
			t.Errorf("Component(%q) = %q, beklenen %q", c.in, got, c.want)
		}
	}
}

func TestComponentEmptyAndDotNames(t *testing.T) {
	for _, in := range []string{"", "   ", ".", "..", "...", ". . .", "\x00\x01"} {
		if got := Component(in); got != "" {
			t.Errorf("Component(%q) = %q, bos bekleniyordu", in, got)
		}
	}
}

// Ayrilmis aygit adlariyla dosya acmak dosya degil AYGIT acar.
// Eslestirme ilk noktadan oncesi uzerinden ve buyuk/kucuk harf duyarsiz.
func TestComponentReservedDeviceNames(t *testing.T) {
	reservedInputs := []string{
		"CON", "con", "Con", "PRN", "AUX", "NUL",
		"COM1", "com9", "LPT1", "lpt9",
		"CONIN$", "conout$",
		"COM¹", "LPT³",
		// Uzantili hali de ayrilmistir: CON.txt da aygita cozulur.
		"CON.txt", "nul.mp4", "com1.tar.gz",
		// Aygit adi cozumlemesinde sondaki bosluklar yok sayilir.
		"CON .txt",
	}
	for _, in := range reservedInputs {
		got := Component(in)
		if !strings.HasPrefix(got, "_") {
			t.Errorf("Component(%q) = %q, ayrilmis ad kacirilmadi", in, got)
		}
	}

	// Ayrilmis OLMAYAN, benzeyen adlar dokunulmamali.
	safe := []string{
		"CONSOLE", "console.txt", "COM", "COM10", "COM0", "LPT0",
		"NULL", "nullable.json", "printer.txt", "auxiliary",
		"my CON file.txt", // ilk noktadan oncesi "my CON file"
	}
	for _, in := range safe {
		if got := Component(in); got != in {
			t.Errorf("Component(%q) = %q, degismemeliydi", in, got)
		}
	}
}

func TestUTF16Len(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"Özgür", 5},      // BMP icinde, her rune 1 birim
		{"日本語", 3},        // BMP icinde
		{"\U0001F600", 2}, // emoji: BMP disi, vekil cift = 2 birim
		{"a\U0001F600b", 4},
	}
	for _, c := range cases {
		if got := utf16Len(c.in); got != c.want {
			t.Errorf("utf16Len(%q) = %d, beklenen %d", c.in, got, c.want)
		}
	}
}

func TestComponentTruncatesToUTF16Limit(t *testing.T) {
	long := strings.Repeat("a", 400) + ".mp4"
	got := Component(long)
	if n := utf16Len(got); n > MaxComponentUTF16 {
		t.Fatalf("uzunluk %d, en fazla %d olmaliydi", n, MaxComponentUTF16)
	}
	if filepath.Ext(got) != ".mp4" {
		t.Errorf("uzanti korunmadi: %q", got)
	}
	if !strings.Contains(got, "~") {
		t.Errorf("kirpma soneki yok: %q", got)
	}
}

// Kirpma DETERMINISTIK olmak zorunda: aksi halde resume ve "zaten var"
// kontrolu koşular arasinda calismaz.
func TestTruncationIsDeterministic(t *testing.T) {
	long := strings.Repeat("b", 500) + ".bin"
	a := Component(long)
	b := Component(long)
	if a != b {
		t.Fatalf("ayni girdi iki farkli cikti verdi:\n%q\n%q", a, b)
	}
}

// Ilk 255 karakteri AYNI olan iki uzun ad, kirpildiginda ayni ada donusurse
// biri digerini ezer. Hash soneki bunu engellemek icin var.
func TestTruncationAvoidsCollisionOnSharedPrefix(t *testing.T) {
	prefix := strings.Repeat("c", 300)
	a := Component(prefix + "-birinci.mp4")
	b := Component(prefix + "-ikinci.mp4")
	if a == b {
		t.Fatalf("iki farkli uzun ad ayni kisa ada dusmus: %q", a)
	}
	if utf16Len(a) > MaxComponentUTF16 || utf16Len(b) > MaxComponentUTF16 {
		t.Fatal("kirpma sinira uymadi")
	}
}

// Emoji iceren uzun ad: vekil cift yarilmamali ve sinir asilmamali.
func TestTruncationDoesNotSplitSurrogatePairs(t *testing.T) {
	long := strings.Repeat("\U0001F600", 300) + ".png"
	got := Component(long)
	if n := utf16Len(got); n > MaxComponentUTF16 {
		t.Fatalf("uzunluk %d, sinir %d", n, MaxComponentUTF16)
	}
	if !utf8Valid(got) {
		t.Fatalf("gecersiz UTF-8 uretildi: %q", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// Patolojik "uzanti" (noktadan sonra cok uzun) butcenin tamamini yememeli.
func TestPathologicalExtensionIsDropped(t *testing.T) {
	long := strings.Repeat("d", 100) + "." + strings.Repeat("e", 300)
	got := Component(long)
	if n := utf16Len(got); n > MaxComponentUTF16 {
		t.Fatalf("uzunluk %d, sinir %d", n, MaxComponentUTF16)
	}
}

// Sinira tam oturan ad kirpilmamali.
func TestExactLimitIsNotTruncated(t *testing.T) {
	exact := strings.Repeat("f", MaxComponentUTF16)
	got := Component(exact)
	if got != exact {
		t.Fatalf("tam sinirdaki ad kirpildi: %d -> %d birim", MaxComponentUTF16, utf16Len(got))
	}
}

// Gercek albumde gorulen bir ad: uzun, ozel karakterli, parantezli.
func TestRealWorldAlbumFilename(t *testing.T) {
	in := "2024-01-02 - Family Therapy - Stella Barey - Play by the Rules (also known as Anal Therapy - Safe Sex with.mp4"
	got := Component(in)
	if got != in {
		t.Errorf("gecerli ad degistirildi:\n%q\n%q", in, got)
	}
}

// Albüm klasör adı da AYNI temizleyiciden gecer; ayri bir kod yolu olmamali.
func TestComponentUsedForDirectoryLabels(t *testing.T) {
	if got := Component("Albüm: 2026 / Yaz"); got != "Albüm- 2026 - Yaz" {
		t.Errorf("klasor adi = %q", got)
	}
	if got := Component("NUL"); got != "_NUL" {
		t.Errorf("ayrilmis klasor adi = %q", got)
	}
}

// Entegrasyon: temizleyici gercekten diske yansiyor mu? Birim testler
// Component'i kanitliyor ama Download'un onu cagirdigini kanitlamiyor.
// Temizleme cagiranda olsaydi bir cagiran atlayabilirdi; bu test o baglantiyi
// sabitliyor.
func TestDownloadAppliesComponentSanitizer(t *testing.T) {
	srv := rangeServer(t, `"v1"`, nil)
	out := tempDir(t)

	cases := []struct {
		rawName, rawDir string
		wantName        string
		wantDir         string
	}{
		// Ayrilmis aygit adi: dosya degil AYGIT acilirdi.
		{"CON.txt", "", "_CON.txt", ""},
		// Yasak karakterler ve sondaki nokta.
		{`kotu:ad?.mp4`, "", "kotu-ad-.mp4", ""},
		{"sondaki nokta.mp4.", "", "sondaki nokta.mp4", ""},
		// Klasor adi da ayni temizleyiciden gecer.
		{"tamam.bin", "Albüm: Yaz / 2026", "tamam.bin", "Albüm- Yaz - 2026"},
	}

	for _, c := range cases {
		d := &Downloader{Client: srv.Client()}
		it := testItem(srv.URL+"/veri.bin", c.rawName)
		it.Dir = c.rawDir
		it.SHA256 = payloadSHA()
		if _, err := d.Download(context.Background(), out, it); err != nil {
			t.Fatalf("Download(%q): %v", c.rawName, err)
		}
		want := filepath.Join(out, c.wantDir, c.wantName)
		if _, err := os.Stat(want); err != nil {
			t.Errorf("beklenen yol yok: %s (%v)", want, err)
		}
	}

	// 255 birim sinirini asan ad gercekten kisaltilmis halde diske inmeli.
	d := &Downloader{Client: srv.Client()}
	long := strings.Repeat("z", 400) + ".mp4"
	it := testItem(srv.URL+"/veri.bin", long)
	it.SHA256 = payloadSHA()
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatalf("uzun ad: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "zzz") {
			found = true
			// ASIL DEGISMEZ: nihai ad + ".part.state" de bilesen sinirina sigmali.
			// Bu entegrasyon testi tam olarak bu hatayi buldu: temizleyici 255'i
			// nihai ada harciyordu ve .part.state 266 birime cikip NTFS
			// tarafindan reddediliyordu.
			if n := utf16Len(e.Name()) + utf16Len(stateSuffix); n > MaxComponentUTF16 {
				t.Errorf("ad + %q = %d birim, sinir %d (%q)",
					stateSuffix, n, MaxComponentUTF16, e.Name())
			}
			if !strings.Contains(e.Name(), "~") {
				t.Errorf("kirpma soneki diske yansimamis: %q", e.Name())
			}
		}
	}
	if !found {
		t.Error("uzun adli dosya diskte bulunamadi")
	}
}
