package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const baseTOML = `
schema_version = 1

[[site]]
name = "pixeldrain"
domains = ["pixeldrain.com", "pixeldra.in"]
cdn_patterns = ["cdn-a"]
canary_urls = ["https://pixeldrain.com/api/misc/rate_limits"]
referer_policy = "none"
user_agent = "siphon/base"
max_concurrent = 3
max_retries = 5
base_delay = "1s"
max_delay = "60s"
max_elapsed = "10m"
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sites.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("temp yazilamadi: %v", err)
	}
	return p
}

func TestLoadEmbeddedOnly(t *testing.T) {
	cfgs, src, err := Load([]byte(baseTOML), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !src.Embedded {
		// cwd'de sites.toml varsa bu test yaniltici olurdu; t.TempDir kullanmiyoruz
		// cunku locate() cwd'ye bakiyor. Repo kokunde kosarsa src dis dosya olur.
		t.Logf("dis config bulundu: %s", src)
	}
	if len(cfgs) != 1 || cfgs[0].Name != "pixeldrain" {
		t.Fatalf("beklenmeyen config: %+v", cfgs)
	}
}

func TestMergeUnionsArraysAndOverwritesScalars(t *testing.T) {
	ext := `
[[site]]
name = "pixeldrain"
domains = ["pixeldrain.tech"]
user_agent = "siphon/override"
`
	cfgs, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfgs[0]

	// Diziler BIRLESIR, ezilmez. Premise 2'nin satis argumani bu:
	// yeni TLD ciktiginda tum listeyi yeniden yazmazsin.
	want := []string{"pixeldrain.com", "pixeldra.in", "pixeldrain.tech"}
	if strings.Join(c.Domains, ",") != strings.Join(want, ",") {
		t.Errorf("domains = %v, beklenen %v (dizi alanlari birlesmeli)", c.Domains, want)
	}
	// Skalerler ezilir.
	if c.UserAgent != "siphon/override" {
		t.Errorf("user_agent = %q, ezilmeliydi", c.UserAgent)
	}
	// Verilmeyen skaler korunur.
	if c.MaxConcurrent != 3 {
		t.Errorf("max_concurrent = %d, temelden korunmaliydi", c.MaxConcurrent)
	}
}

func TestDomainsRemoveIsExplicit(t *testing.T) {
	ext := `
[[site]]
name = "pixeldrain"
domains_remove = ["pixeldra.in"]
`
	cfgs, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, d := range cfgs[0].Domains {
		if d == "pixeldra.in" {
			t.Fatal("domains_remove uygulanmadi")
		}
	}
	if len(cfgs[0].Domains) != 1 {
		t.Fatalf("domains = %v, sadece pixeldrain.com kalmaliydi", cfgs[0].Domains)
	}
}

func TestExternalCanAddNewSite(t *testing.T) {
	ext := `
[[site]]
name = "yenisite"
domains = ["ornek.com"]
`
	cfgs, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfgs) != 2 || cfgs[1].Name != "yenisite" {
		t.Fatalf("yeni site eklenmedi: %+v", cfgs)
	}
}

func TestSchemaVersionMismatchIsHardFail(t *testing.T) {
	ext := `
schema_version = 99
[[site]]
name = "pixeldrain"
domains = ["x.com"]
`
	_, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err == nil {
		t.Fatal("schema_version uyusmazligi hata vermeliydi")
	}
	if !errors.Is(err, ErrUsage) {
		t.Errorf("ErrUsage bekleniyordu (cikis kodu 3), %v geldi", err)
	}
}

func TestInvalidTOMLIsHardFailNotFallback(t *testing.T) {
	_, _, err := Load([]byte(baseTOML), writeTemp(t, "bu gecerli toml degil ==="))
	if err == nil {
		t.Fatal("gecersiz TOML hata vermeliydi; sessizce gomuluye dusmek teshisi yok eder")
	}
	if !errors.Is(err, ErrUsage) {
		t.Errorf("ErrUsage bekleniyordu, %v geldi", err)
	}
}

func TestMissingExplicitConfigIsUsageError(t *testing.T) {
	_, _, err := Load([]byte(baseTOML), filepath.Join(t.TempDir(), "yok.toml"))
	if err == nil {
		t.Fatal("-c ile verilen dosya yoksa hata vermeli, sessizce gomuluye dusmemeli")
	}
	if !errors.Is(err, ErrUsage) {
		t.Errorf("ErrUsage bekleniyordu, %v geldi", err)
	}
}

func TestEmbeddedMissingSchemaVersion(t *testing.T) {
	_, _, err := Load([]byte("[[site]]\nname=\"a\"\ndomains=[\"a.com\"]\n"), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("gomulu kopyada schema_version zorunlu olmali, err=%v", err)
	}
}

func TestDurationsParsed(t *testing.T) {
	cfgs, _, err := Load([]byte(baseTOML), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfgs[0]
	if c.BaseDelay != time.Second || c.MaxDelay != 60*time.Second || c.MaxElapsed != 10*time.Minute {
		t.Fatalf("sureler yanlis: %v %v %v", c.BaseDelay, c.MaxDelay, c.MaxElapsed)
	}
}

func TestBadDurationIsUsageError(t *testing.T) {
	bad := strings.Replace(baseTOML, `base_delay = "1s"`, `base_delay = "bir saniye"`, 1)
	_, _, err := Load([]byte(bad), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("gecersiz sure ErrUsage vermeli, err=%v", err)
	}
}

func TestBadRefererPolicyIsUsageError(t *testing.T) {
	bad := strings.Replace(baseTOML, `referer_policy = "none"`, `referer_policy = "her zaman"`, 1)
	_, _, err := Load([]byte(bad), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("gecersiz referer_policy ErrUsage vermeli, err=%v", err)
	}
}

func TestEmptyDomainsIsUsageError(t *testing.T) {
	_, _, err := Load([]byte("schema_version = 1\n[[site]]\nname=\"a\"\n"), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("domains bos olamaz, err=%v", err)
	}
}

func TestDuplicateSiteNameIsUsageError(t *testing.T) {
	dup := baseTOML + "\n[[site]]\nname = \"pixeldrain\"\ndomains = [\"x.com\"]\n"
	_, _, err := Load([]byte(dup), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("ayni ad iki kez tanimlanamaz, err=%v", err)
	}
}
