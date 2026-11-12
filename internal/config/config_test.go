package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/testutil"
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

// chdirTemp moves the working directory into an empty folder and returns it.
// chdir affects the whole process, so tests using it must NOT run in parallel.
func chdirTemp(t *testing.T) string {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := testutil.TempDir(t)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return dir
}

// A sites.toml in the working directory is NOT read. The working directory
// is the command line's default output folder: a downloaded file named
// sites.toml would be laid over the site definitions on the next run, and it
// can name the programs Siphon runs (the direct entry's tool paths). The
// tests in this package also run where the repo's own sites.toml lies.
func TestWorkingDirectorySitesTomlIsIgnored(t *testing.T) {
	dir := chdirTemp(t)
	planted := baseTOML + "\n[[site]]\nname = \"direct\"\ndomains = []\n[site.extra]\nyt_dlp = \"not-this.exe\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sites.toml"), []byte(planted), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgs, src, err := Load([]byte(baseTOML), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !src.Embedded {
		t.Fatalf("the working directory's sites.toml was read: %s", src)
	}
	for _, c := range cfgs {
		if c.Name == "direct" {
			t.Fatal("a site from the working directory's sites.toml was added")
		}
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(testutil.TempDir(t), "sites.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("could not write temp file: %v", err)
	}
	return p
}

func TestLoadEmbeddedOnly(t *testing.T) {
	cfgs, src, err := Load([]byte(baseTOML), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Only -c and the folder of the test binary are searched, and neither
	// has a sites.toml, so this is a firm claim.
	if !src.Embedded {
		t.Fatalf("embedded copy was not used: %s", src)
	}
	if len(cfgs) != 1 || cfgs[0].Name != "pixeldrain" {
		t.Fatalf("unexpected config: %+v", cfgs)
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

	// Arrays are MERGED, not overwritten. This is Premise 2's selling point:
	// when a new TLD appears you don't rewrite the whole list.
	want := []string{"pixeldrain.com", "pixeldra.in", "pixeldrain.tech"}
	if strings.Join(c.Domains, ",") != strings.Join(want, ",") {
		t.Errorf("domains = %v, want %v (array fields must be merged)", c.Domains, want)
	}
	// Scalars are overwritten.
	if c.UserAgent != "siphon/override" {
		t.Errorf("user_agent = %q, should have been overwritten", c.UserAgent)
	}
	// A scalar that isn't given is kept.
	if c.MaxConcurrent != 3 {
		t.Errorf("max_concurrent = %d, should have been kept from the base", c.MaxConcurrent)
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
			t.Fatal("domains_remove was not applied")
		}
	}
	if len(cfgs[0].Domains) != 1 {
		t.Fatalf("domains = %v, only pixeldrain.com should remain", cfgs[0].Domains)
	}
}

func TestExternalCanAddNewSite(t *testing.T) {
	ext := `
[[site]]
name = "newsite"
domains = ["example.com"]
`
	cfgs, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfgs) != 2 || cfgs[1].Name != "newsite" {
		t.Fatalf("new site was not added: %+v", cfgs)
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
		t.Fatal("a schema_version mismatch should have failed")
	}
	if !errors.Is(err, ErrUsage) {
		t.Errorf("expected ErrUsage (exit code 3), got %v", err)
	}
}

func TestInvalidTOMLIsHardFailNotFallback(t *testing.T) {
	_, _, err := Load([]byte(baseTOML), writeTemp(t, "this is not valid toml ==="))
	if err == nil {
		t.Fatal("invalid TOML should fail; silently falling back to the embedded copy destroys diagnosability")
	}
	if !errors.Is(err, ErrUsage) {
		t.Errorf("expected ErrUsage, got %v", err)
	}
}

func TestMissingExplicitConfigIsUsageError(t *testing.T) {
	_, _, err := Load([]byte(baseTOML), filepath.Join(testutil.TempDir(t), "missing.toml"))
	if err == nil {
		t.Fatal("a file given with -c that doesn't exist must fail, not silently fall back to the embedded copy")
	}
	if !errors.Is(err, ErrUsage) {
		t.Errorf("expected ErrUsage, got %v", err)
	}
}

func TestEmbeddedMissingSchemaVersion(t *testing.T) {
	_, _, err := Load([]byte("[[site]]\nname=\"a\"\ndomains=[\"a.com\"]\n"), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("schema_version must be mandatory in the embedded copy, err=%v", err)
	}
}

func TestDurationsParsed(t *testing.T) {
	cfgs, _, err := Load([]byte(baseTOML), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfgs[0]
	if c.BaseDelay != time.Second || c.MaxDelay != 60*time.Second || c.MaxElapsed != 10*time.Minute {
		t.Fatalf("wrong durations: %v %v %v", c.BaseDelay, c.MaxDelay, c.MaxElapsed)
	}
}

func TestBadDurationIsUsageError(t *testing.T) {
	bad := strings.Replace(baseTOML, `base_delay = "1s"`, `base_delay = "one second"`, 1)
	_, _, err := Load([]byte(bad), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("an invalid duration must give ErrUsage, err=%v", err)
	}
}

func TestBadRefererPolicyIsUsageError(t *testing.T) {
	bad := strings.Replace(baseTOML, `referer_policy = "none"`, `referer_policy = "always"`, 1)
	_, _, err := Load([]byte(bad), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("an invalid referer_policy must give ErrUsage, err=%v", err)
	}
}

func TestEmptyDomainsIsUsageError(t *testing.T) {
	_, _, err := Load([]byte("schema_version = 1\n[[site]]\nname=\"a\"\n"), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("domains cannot be empty, err=%v", err)
	}
}

func TestDuplicateSiteNameIsUsageError(t *testing.T) {
	dup := baseTOML + "\n[[site]]\nname = \"pixeldrain\"\ndomains = [\"x.com\"]\n"
	_, _, err := Load([]byte(dup), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("the same name cannot be defined twice, err=%v", err)
	}
}

// The bunkr endpoints in the embedded config must be COMPLETE.
//
// ExtraOr silently falls back to the in-code default for a missing/empty key;
// so a typo like "sign_endpont" would be ignored without any symptom and you
// would believe the fix you wrote into the config was applied.
func TestEmbeddedBunkrExtrasAreComplete(t *testing.T) {
	cfgs, _, err := Load(Embedded, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var extra map[string]string
	for _, c := range cfgs {
		if c.Name == "bunkr" {
			extra = c.Extra
		}
	}
	if extra == nil {
		t.Fatal("bunkr is missing from the embedded config")
	}
	for _, k := range []string{"api_endpoint", "fallback_api_endpoint", "dl_origin", "sign_endpoint"} {
		if v := extra[k]; !strings.HasPrefix(v, "https://") {
			t.Errorf("%s = %q; a missing or wrong key silently falls back to the default", k, v)
		}
	}
	if p := extra["legacy_path_prefix"]; !strings.HasPrefix(p, "/") {
		t.Errorf("legacy_path_prefix = %q, must start with '/'", p)
	}
}

// max_segments is a per-site CEILING; the embedded config has deliberate values.
func TestEmbeddedMaxSegments(t *testing.T) {
	cfgs, _, err := Load(Embedded, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"pixeldrain": 1, "bunkr": 1, "mega": 8, "cyberfile": 1}
	for _, c := range cfgs {
		if w, ok := want[c.Name]; ok && c.MaxSegments != w {
			t.Errorf("%s max_segments = %d, want %d", c.Name, c.MaxSegments, w)
		}
	}
}

// max_connections: the embedded values, the default when unset (files x
// connections per file), and an external file overriding it like any scalar.
func TestMaxConnections(t *testing.T) {
	cfgs, _, err := Load(Embedded, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"pixeldrain": 3, "bunkr": 4, "mega": 32, "cyberfile": 1}
	for _, c := range cfgs {
		if w, ok := want[c.Name]; ok && c.MaxConnections != w {
			t.Errorf("%s max_connections = %d, want %d", c.Name, c.MaxConnections, w)
		}
		if c.MaxConnections < c.MaxConcurrent {
			t.Errorf("%s: max_connections %d is below max_concurrent %d; a file couldn't have its own connection",
				c.Name, c.MaxConnections, c.MaxConcurrent)
		}
	}

	ext := `
schema_version = 1
[[site]]
name = "pixeldrain"
max_segments = 4
max_connections = 10
`
	cfgs, _, err = Load([]byte(baseTOML), writeTemp(t, ext))
	if err != nil {
		t.Fatal(err)
	}
	if c := cfgs[0]; c.MaxConnections != 10 || c.MaxSegments != 4 {
		t.Errorf("override: max_connections %d, max_segments %d; want 10, 4", c.MaxConnections, c.MaxSegments)
	}
}

// host_limits: a pool of download hosts and how many files it gives at once.
// An external file changes one pattern without repeating the others, and 0
// lifts an embedded limit.
func TestHostLimitsParseAndMerge(t *testing.T) {
	base := baseTOML + "host_limits = { \"*.slow.test\" = 1, \"*.mid.test\" = 2 }\n"
	cfgs, _, err := Load([]byte(base), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfgs[0].HostLimits; len(got) != 2 || got["*.slow.test"] != 1 || got["*.mid.test"] != 2 {
		t.Fatalf("host limits = %v", got)
	}

	ext := filepath.Join(testutil.TempDir(t), "sites.toml")
	over := "[[site]]\nname = \"pixeldrain\"\nhost_limits = { \"*.slow.test\" = 0, \"*.MID.test\" = 3, \"*.new.test\" = 1 }\n"
	if err := os.WriteFile(ext, []byte(over), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgs, _, err = Load([]byte(base), ext)
	if err != nil {
		t.Fatalf("Load with an external file: %v", err)
	}
	got := cfgs[0].HostLimits
	if _, still := got["*.slow.test"]; still {
		t.Errorf("a limit set to 0 is still there: %v", got)
	}
	if got["*.new.test"] != 1 {
		t.Errorf("the external file's new pattern is missing: %v", got)
	}
	if got["*.mid.test"] != 3 {
		t.Errorf("a pattern written in another case isn't the same pattern: %v", got)
	}
}

func TestNegativeHostLimitIsUsageError(t *testing.T) {
	_, _, err := Load([]byte(baseTOML+"host_limits = { \"*.slow.test\" = -1 }\n"), "")
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

// The embedded bunkr entry: several files at once, one from *.scdn.st.
func TestEmbeddedBunkrLimits(t *testing.T) {
	cfgs, _, err := Load(Embedded, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfgs {
		if c.Name != "bunkr" {
			continue
		}
		if c.MaxConcurrent != 4 || c.MaxSegments != 1 || c.HostLimits["*.scdn.st"] != 1 {
			t.Errorf("bunkr: max_concurrent %d, max_segments %d, host_limits %v", c.MaxConcurrent, c.MaxSegments, c.HostLimits)
		}
		if len(c.CanaryURLs) == 0 || len(c.Extra) == 0 {
			t.Errorf("bunkr lost its canaries or extra settings: %d, %d", len(c.CanaryURLs), len(c.Extra))
		}
		return
	}
	t.Fatal("no bunkr entry")
}

// A site's DNS and proxy: a known resolver's name becomes its address, an
// external file sets or changes them, and a value Siphon couldn't use is a
// configuration error at start, not a failed download later.
func TestDNSAndProxySettings(t *testing.T) {
	cfgs, _, err := Load([]byte(baseTOML+"dns_resolver = \"quad9\"\nproxy = \"socks5://127.0.0.1:1080\"\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if c := cfgs[0]; c.DNSResolver != "https://9.9.9.9/dns-query" || c.Proxy != "socks5://127.0.0.1:1080" {
		t.Errorf("dns %q, proxy %q", c.DNSResolver, c.Proxy)
	}

	ext := filepath.Join(testutil.TempDir(t), "sites.toml")
	if err := os.WriteFile(ext, []byte("[[site]]\nname = \"pixeldrain\"\nproxy = \"http://10.0.0.2:3128\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgs, _, err = Load([]byte(baseTOML+"dns_resolver = \"quad9\"\nproxy = \"socks5://127.0.0.1:1080\"\n"), ext)
	if err != nil {
		t.Fatal(err)
	}
	if c := cfgs[0]; c.Proxy != "http://10.0.0.2:3128" || c.DNSResolver != "https://9.9.9.9/dns-query" {
		t.Errorf("after the external file: dns %q, proxy %q", c.DNSResolver, c.Proxy)
	}

	for _, bad := range []string{"dns_resolver = \"8.8.8.8\"\n", "proxy = \"127.0.0.1:1080\"\n", "proxy = \"socks4://127.0.0.1:1080\"\n"} {
		if _, _, err := Load([]byte(baseTOML+bad), ""); !errors.Is(err, ErrUsage) {
			t.Errorf("%q: err = %v, want a usage error", bad, err)
		}
	}
}
