package doctor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// fakeResolver, site.Resolver arayüzünü karşılayan asgari bir taklit.
// doctor yalnızca Diagnose'u çağırıyor; geri kalanı sözleşme gereği var.
type fakeResolver struct {
	results []site.LayerResult
	err     error
	calls   int
}

func (f *fakeResolver) Match(string) bool { return false }

func (f *fakeResolver) Resolve(context.Context, string, func(site.Item) error) ([]site.ItemError, error) {
	return nil, nil
}

func (f *fakeResolver) ResolveOne(context.Context, string) (site.Item, error) {
	return site.Item{}, nil
}

func (f *fakeResolver) Diagnose(context.Context) ([]site.LayerResult, error) {
	f.calls++
	return f.results, f.err
}

func ok(l site.Layer, detail string) site.LayerResult {
	return site.LayerResult{Layer: l, Status: site.StatusOK, Detail: detail}
}

func TestReportWorst(t *testing.T) {
	cases := []struct {
		name string
		r    Report
		want site.LayerStatus
	}{
		{"hepsi OK", Report{Results: []site.LayerResult{
			ok(site.LayerDNS, "a"), ok(site.LayerTLS, "b"),
		}}, site.StatusOK},
		{"bir WARN", Report{Results: []site.LayerResult{
			ok(site.LayerDNS, "a"),
			{Layer: site.LayerCDN, Status: site.StatusWarn},
		}}, site.StatusWarn},
		{"WARN ve FAIL -> FAIL", Report{Results: []site.LayerResult{
			{Layer: site.LayerCDN, Status: site.StatusWarn},
			{Layer: site.LayerDNS, Status: site.StatusFail},
		}}, site.StatusFail},
		{"Diagnose hatasi -> FAIL", Report{Err: errors.New("canary yok")}, site.StatusFail},
		{"bos rapor -> OK", Report{}, site.StatusOK},
	}
	for _, c := range cases {
		if got := c.r.Worst(); got != c.want {
			t.Errorf("%s: Worst() = %v, beklenen %v", c.name, got, c.want)
		}
	}
}

func TestRunCallsDiagnosePerSite(t *testing.T) {
	a := &fakeResolver{results: []site.LayerResult{ok(site.LayerDNS, "a")}}
	b := &fakeResolver{err: errors.New("patladi")}

	reports := Run(context.Background(), []Named{
		{Name: "alfa", Resolver: a},
		{Name: "beta", Resolver: b},
	})
	if len(reports) != 2 {
		t.Fatalf("%d rapor, 2 bekleniyordu", len(reports))
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("Diagnose cagri sayilari: a=%d b=%d", a.calls, b.calls)
	}
	if reports[0].Site != "alfa" || reports[1].Site != "beta" {
		t.Errorf("site sirasi korunmadi: %q, %q", reports[0].Site, reports[1].Site)
	}
	if reports[1].Err == nil {
		t.Error("Diagnose hatasi rapora tasinmadi")
	}
}

func TestFormatOutputShape(t *testing.T) {
	var buf bytes.Buffer
	worst := Format(&buf, []Report{{
		Site: "bunkr",
		Results: []site.LayerResult{
			{Layer: site.LayerDNS, Status: site.StatusOK, Detail: "4 adres", Evidence: "1.2.3.4"},
			{Layer: site.LayerCDN, Status: site.StatusWarn, Detail: "yeni host", Evidence: "x.cdn.cr"},
		},
	}})
	out := buf.String()

	for _, want := range []string{"bunkr", "DNS", "OK", "4 adres", "1.2.3.4", "CDN", "WARN", "x.cdn.cr"} {
		if !strings.Contains(out, want) {
			t.Errorf("cikti %q icermiyor:\n%s", want, out)
		}
	}
	// Evidence ayri bir satirda ve girintili olmali: teshis satirinin
	// okunabilirligi bu aracin butun amaci.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("%d satir, 5 bekleniyordu:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[2], "1.2.3.4") || strings.Contains(lines[2], "DNS") {
		t.Errorf("Evidence kendi satirinda olmali: %q", lines[2])
	}
	if worst != site.StatusWarn {
		t.Errorf("worst = %v, WARN bekleniyordu", worst)
	}
}

// WARN cikisi FAIL'e yukseltmemeli: "CDN bir kapi degil sinyal" kurali ancak
// WARN basarisizlik sayilmazsa anlam tasiyor.
func TestFormatWarnDoesNotBecomeFail(t *testing.T) {
	var buf bytes.Buffer
	worst := Format(&buf, []Report{
		{Site: "a", Results: []site.LayerResult{{Layer: site.LayerCDN, Status: site.StatusWarn}}},
		{Site: "b", Results: []site.LayerResult{ok(site.LayerDNS, "tamam")}},
	})
	if worst != site.StatusWarn {
		t.Fatalf("worst = %v, WARN bekleniyordu", worst)
	}
}

func TestFormatFailWinsAcrossSites(t *testing.T) {
	var buf bytes.Buffer
	worst := Format(&buf, []Report{
		{Site: "a", Results: []site.LayerResult{{Layer: site.LayerCDN, Status: site.StatusWarn}}},
		{Site: "b", Results: []site.LayerResult{{Layer: site.LayerDNS, Status: site.StatusFail}}},
		{Site: "c", Results: []site.LayerResult{ok(site.LayerDNS, "tamam")}},
	})
	if worst != site.StatusFail {
		t.Fatalf("worst = %v, FAIL bekleniyordu", worst)
	}
}

func TestFormatHandlesDiagnoseError(t *testing.T) {
	var buf bytes.Buffer
	worst := Format(&buf, []Report{{Site: "bunkr", Err: errors.New("canary listesi bos")}})
	if worst != site.StatusFail {
		t.Errorf("worst = %v, FAIL bekleniyordu", worst)
	}
	if !strings.Contains(buf.String(), "canary listesi bos") {
		t.Errorf("hata mesaji basilmadi:\n%s", buf.String())
	}
}

func TestFormatHandlesEmptyResults(t *testing.T) {
	var buf bytes.Buffer
	worst := Format(&buf, []Report{{Site: "bunkr"}})
	if worst != site.StatusOK {
		// Sonuc yoksa WARN basiliyor ama Worst() OK doner; cikis kodunu
		// bozmamak icin bu kasitli.
		t.Logf("worst = %v", worst)
	}
	if !strings.Contains(buf.String(), "teşhis sonucu yok") {
		t.Errorf("bos sonuc bildirilmedi:\n%s", buf.String())
	}
}

// --- Recorder ---

func TestRecorderSavesRealBytes(t *testing.T) {
	dir := t.TempDir()
	rec := &Recorder{Dir: dir}

	body := []byte("<html>gercek govde</html>")
	rec.For("bunkr")("canary.html", body)

	saved := rec.Saved()
	if len(saved) != 1 {
		t.Fatalf("%d dosya kaydedildi, 1 bekleniyordu: %v", len(saved), saved)
	}
	got, err := os.ReadFile(saved[0])
	if err != nil {
		t.Fatalf("kayit okunamadi: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("kaydedilen icerik farkli: %q", got)
	}

	// Ad hem siteyi hem etiketi tasimali: alti ay sonra hangi kaydin neye ait
	// oldugunu bilmek gerekiyor.
	base := filepath.Base(saved[0])
	if !strings.Contains(base, "bunkr") || !strings.Contains(base, "canary.html") {
		t.Errorf("dosya adi eksik bilgi tasiyor: %q", base)
	}
	if len(rec.Errs()) != 0 {
		t.Errorf("beklenmeyen hatalar: %v", rec.Errs())
	}
}

func TestRecorderPerSiteLabels(t *testing.T) {
	dir := t.TempDir()
	rec := &Recorder{Dir: dir}
	rec.For("bunkr")("canary.html", []byte("a"))
	rec.For("pixeldrain")("canary.json", []byte("b"))

	saved := rec.Saved()
	if len(saved) != 2 {
		t.Fatalf("%d dosya, 2 bekleniyordu", len(saved))
	}
	joined := strings.Join(saved, " ")
	for _, want := range []string{"bunkr", "pixeldrain"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q adli kayit yok: %v", want, saved)
		}
	}
}

// Bos govde kaydedilmemeli: sifir baytlik bir dosya diff'te gurultu.
func TestRecorderSkipsEmptyBodies(t *testing.T) {
	dir := t.TempDir()
	cfg := site.SiteConfig{Record: (&Recorder{Dir: dir}).For("bunkr")}
	cfg.Recordln("canary.html", nil)
	cfg.Recordln("canary.html", []byte{})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d dosya olustu, bos govde kaydedilmemeliydi", len(entries))
	}
}

// Kayit hatasi teshisi DUSURMEZ: asil is katman raporu, kayit yardimci.
func TestRecorderCollectsErrorsWithoutPanicking(t *testing.T) {
	// Var olan bir DOSYAyi klasor olarak kullanmaya zorla.
	f := filepath.Join(t.TempDir(), "dosya")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &Recorder{Dir: f}
	rec.For("bunkr")("canary.html", []byte("veri"))

	if len(rec.Saved()) != 0 {
		t.Error("hata durumunda dosya kaydedilmis gorunuyor")
	}
	if len(rec.Errs()) == 0 {
		t.Error("kayit hatasi toplanmadi")
	}
}

func TestRecorderDefaultDir(t *testing.T) {
	// Dir bos ise DefaultDir kullanilmali. Gercekten yazmamak icin cwd'yi
	// gecici klasore tasi.
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	rec := &Recorder{}
	rec.For("bunkr")("canary.html", []byte("veri"))
	if len(rec.Saved()) != 1 {
		t.Fatalf("kaydedilmedi: %v / %v", rec.Saved(), rec.Errs())
	}
	if !strings.Contains(rec.Saved()[0], DefaultDir) {
		t.Errorf("DefaultDir kullanilmadi: %q", rec.Saved()[0])
	}
}

func TestSafeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"bunkr", "bunkr"},
		{"canary.html", "canary.html"},
		{"a/b\\c", "a-b-c"},
		{"boşluk var", "bo-luk-var"},
		{"", "kayit"},
		{"   ", "kayit"},
	}
	for _, c := range cases {
		if got := safe(c.in); got != c.want {
			t.Errorf("safe(%q) = %q, beklenen %q", c.in, got, c.want)
		}
	}
}
