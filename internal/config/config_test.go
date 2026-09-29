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

// isolatedCwd moves the working directory into an empty folder.
//
// Why it is needed: sites.toml now lives under internal/config (so both
// binaries can use the same embedded copy). Tests run in this package's
// directory, so locate() finds THAT file in the cwd and tests that expect
// "the embedded copy is used" silently read the external file instead.
// Without isolation those tests don't measure the right thing.
//
// chdir affects the whole process, so these tests must NOT run in parallel.
func isolatedCwd(t *testing.T) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sites.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("could not write temp file: %v", err)
	}
	return p
}

func TestLoadEmbeddedOnly(t *testing.T) {
	isolatedCwd(t)
	cfgs, src, err := Load([]byte(baseTOML), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// There can be no external config in an isolated cwd, so this is now a
	// firm claim.
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
	_, _, err := Load([]byte(baseTOML), filepath.Join(t.TempDir(), "missing.toml"))
	if err == nil {
		t.Fatal("a file given with -c that doesn't exist must fail, not silently fall back to the embedded copy")
	}
	if !errors.Is(err, ErrUsage) {
		t.Errorf("expected ErrUsage, got %v", err)
	}
}

func TestEmbeddedMissingSchemaVersion(t *testing.T) {
	isolatedCwd(t)
	_, _, err := Load([]byte("[[site]]\nname=\"a\"\ndomains=[\"a.com\"]\n"), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("schema_version must be mandatory in the embedded copy, err=%v", err)
	}
}

func TestDurationsParsed(t *testing.T) {
	isolatedCwd(t)
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
	isolatedCwd(t)
	bad := strings.Replace(baseTOML, `base_delay = "1s"`, `base_delay = "one second"`, 1)
	_, _, err := Load([]byte(bad), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("an invalid duration must give ErrUsage, err=%v", err)
	}
}

func TestBadRefererPolicyIsUsageError(t *testing.T) {
	isolatedCwd(t)
	bad := strings.Replace(baseTOML, `referer_policy = "none"`, `referer_policy = "always"`, 1)
	_, _, err := Load([]byte(bad), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("an invalid referer_policy must give ErrUsage, err=%v", err)
	}
}

func TestEmptyDomainsIsUsageError(t *testing.T) {
	isolatedCwd(t)
	_, _, err := Load([]byte("schema_version = 1\n[[site]]\nname=\"a\"\n"), "")
	if err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("domains cannot be empty, err=%v", err)
	}
}

func TestDuplicateSiteNameIsUsageError(t *testing.T) {
	isolatedCwd(t)
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
	isolatedCwd(t)
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
	isolatedCwd(t)
	cfgs, _, err := Load(Embedded, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"pixeldrain": 1, "bunkr": 3, "mega": 8}
	for _, c := range cfgs {
		if w, ok := want[c.Name]; ok && c.MaxSegments != w {
			t.Errorf("%s max_segments = %d, want %d", c.Name, c.MaxSegments, w)
		}
	}
}

// max_connections: the embedded values, the default when unset (files x
// connections per file), and an external file overriding it like any scalar.
func TestMaxConnections(t *testing.T) {
	isolatedCwd(t)
	cfgs, _, err := Load(Embedded, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"pixeldrain": 3, "bunkr": 6, "mega": 32}
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
