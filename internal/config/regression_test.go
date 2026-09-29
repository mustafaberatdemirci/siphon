package config

// Regression tests for config bugs found in code review.

import (
	"strings"
	"testing"
)

// FINDING 10a. remove() compared raw strings, while the matching side
// (site.MatchHost) lowercases the host and drops a trailing dot. Result: a
// removal written as `domains_remove = ["PixelDrain.COM"]` silently did
// nothing.
func TestDomainsRemoveIsCaseAndDotInsensitive(t *testing.T) {
	cases := []string{"PixelDrain.COM", "pixeldrain.com.", "  PIXELDRAIN.com  "}
	for _, form := range cases {
		ext := "[[site]]\nname = \"pixeldrain\"\ndomains_remove = [\"" + form + "\"]\n"
		cfgs, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
		if err != nil {
			t.Fatalf("%q: Load: %v", form, err)
		}
		for _, d := range cfgs[0].Domains {
			if strings.EqualFold(strings.TrimSuffix(d, "."), "pixeldrain.com") {
				t.Errorf("removal with %q did not work; domains = %v", form, cfgs[0].Domains)
			}
		}
	}
}

// FINDING 10b. Removal was applied inside mergeSite, but a site NEWLY defined
// by the external file never enters that branch. Result: the new site's own
// domains_remove was silently ignored.
func TestNewSiteOwnDomainsRemoveIsApplied(t *testing.T) {
	ext := `
[[site]]
name = "newsite"
domains = ["one.com", "two.com", "three.com"]
domains_remove = ["two.com"]
`
	cfgs, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var found *struct{ domains []string }
	for _, c := range cfgs {
		if c.Name == "newsite" {
			found = &struct{ domains []string }{c.Domains}
		}
	}
	if found == nil {
		t.Fatal("newsite was not built")
	}
	for _, d := range found.domains {
		if d == "two.com" {
			t.Fatalf("the new site's domains_remove was not applied: %v", found.domains)
		}
	}
	if len(found.domains) != 2 {
		t.Fatalf("domains = %v, expected two entries", found.domains)
	}
}

// If removal sweeps away every domain that is a usage error: a resolver with
// empty domains matches no URL and the tool silently downloads nothing.
func TestRemovingAllDomainsIsUsageError(t *testing.T) {
	ext := `
[[site]]
name = "pixeldrain"
domains_remove = ["pixeldrain.com", "pixeldra.in"]
`
	_, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err == nil {
		t.Fatal("expected an error when every domain is removed")
	}
}

// cdn_patterns_remove must go through the same path.
func TestCDNPatternsRemoveApplied(t *testing.T) {
	ext := `
[[site]]
name = "pixeldrain"
cdn_patterns = ["cdn-b"]
cdn_patterns_remove = ["CDN-A"]
`
	cfgs, _, err := Load([]byte(baseTOML), writeTemp(t, ext))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range cfgs[0].CDNPatterns {
		if strings.EqualFold(c, "cdn-a") {
			t.Fatalf("cdn_patterns_remove was not applied: %v", cfgs[0].CDNPatterns)
		}
	}
	if len(cfgs[0].CDNPatterns) != 1 || cfgs[0].CDNPatterns[0] != "cdn-b" {
		t.Fatalf("cdn_patterns = %v, only cdn-b should remain", cfgs[0].CDNPatterns)
	}
}
