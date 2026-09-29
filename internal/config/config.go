// Package config is responsible for loading and merging sites.toml.
//
// The original file layout had no separate config package; merge logic was
// left to main.go. The reason for a package of its own: union merge,
// domains_remove and schema_version validation are real logic that deserves
// table tests, and keeping them in the same file as flag parsing makes both
// harder.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// SchemaVersion is the schema version this binary understands.
const SchemaVersion = 1

// ErrUsage is the error class that maps to exit code 3: invalid TOML,
// schema_version mismatch, unreadable file. It is not to be confused with
// "something happened on the site side" (exit 2); the diagnosis paths are
// completely different.
var ErrUsage = errors.New("configuration error")

type file struct {
	SchemaVersion *int      `toml:"schema_version"`
	Sites         []rawSite `toml:"site"`
}

type rawSite struct {
	Name string `toml:"name"`

	Domains       []string `toml:"domains"`
	LegacyDomains []string `toml:"legacy_domains"`
	MatchPatterns []string `toml:"match_patterns"`
	CDNPatterns   []string `toml:"cdn_patterns"`
	CanaryURLs    []string `toml:"canary_urls"`

	// Removal must be explicit. Since array fields are merged, there is no
	// other way to take a domain out of the list.
	DomainsRemove     []string `toml:"domains_remove"`
	CDNPatternsRemove []string `toml:"cdn_patterns_remove"`

	UserAgent      string `toml:"user_agent"`
	RefererPolicy  string `toml:"referer_policy"`
	MaxConcurrent  int    `toml:"max_concurrent"`
	MaxSegments    int    `toml:"max_segments"`
	MaxConnections int    `toml:"max_connections"`
	DNSResolver    string `toml:"dns_resolver"`

	MaxRetries int    `toml:"max_retries"`
	BaseDelay  string `toml:"base_delay"`
	MaxDelay   string `toml:"max_delay"`
	MaxElapsed string `toml:"max_elapsed"`

	// Extra holds site-specific key/value settings (like bunkr's api_endpoint).
	Extra map[string]string `toml:"extra"`
}

// Source says where the configuration came from. It is printed with -v;
// "which config was loaded" is the first step of any diagnosis.
type Source struct {
	Path     string // empty for the embedded copy
	Embedded bool
}

func (s Source) String() string {
	if s.Embedded {
		return "embedded"
	}
	return s.Path
}

// Load reads the embedded copy and overlays the external copy if one is found.
//
// Search order: explicitPath -> next to the exe -> cwd. The first one found is
// used. If explicitPath is given but the file does not exist that is a usage
// error; silently falling back to the embedded copy makes diagnosis
// impossible.
func Load(embedded []byte, explicitPath string) ([]site.SiteConfig, Source, error) {
	base, err := parse(embedded, "embedded sites.toml")
	if err != nil {
		return nil, Source{}, err
	}
	if base.SchemaVersion == nil {
		return nil, Source{}, fmt.Errorf("%w: embedded sites.toml has no schema_version", ErrUsage)
	}
	if *base.SchemaVersion != SchemaVersion {
		return nil, Source{}, fmt.Errorf("%w: embedded schema_version=%d, this binary expects %d",
			ErrUsage, *base.SchemaVersion, SchemaVersion)
	}

	path, err := locate(explicitPath)
	if err != nil {
		return nil, Source{}, err
	}
	if path == "" {
		cfgs, err := build(base)
		return cfgs, Source{Embedded: true}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Source{}, fmt.Errorf("%w: could not read %s: %v", ErrUsage, path, err)
	}
	ext, err := parse(data, path)
	if err != nil {
		return nil, Source{}, err
	}
	// schema_version is optional in the external file; if given it must match.
	if ext.SchemaVersion != nil && *ext.SchemaVersion != SchemaVersion {
		return nil, Source{}, fmt.Errorf("%w: %s schema_version=%d, this binary expects %d",
			ErrUsage, path, *ext.SchemaVersion, SchemaVersion)
	}

	cfgs, err := build(merge(base, ext))
	return cfgs, Source{Path: path}, err
}

// locate looks for the external config file. Returns an empty path if none
// is found (the embedded copy is used).
func locate(explicitPath string) (string, error) {
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err != nil {
			return "", fmt.Errorf("%w: could not open %s given with -c: %v", ErrUsage, explicitPath, err)
		}
		return explicitPath, nil
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "sites.toml"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "sites.toml"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", nil
}

func parse(data []byte, label string) (file, error) {
	var f file
	// Invalid TOML is a hard fail: silently falling back to the embedded copy
	// destroys the diagnosability that is the whole point of the tool.
	if _, err := toml.Decode(string(data), &f); err != nil {
		return file{}, fmt.Errorf("%w: %s is not valid TOML: %v", ErrUsage, label, err)
	}
	return f, nil
}

// merge overlays the external file on top of the base.
func merge(base, ext file) file {
	out := file{SchemaVersion: base.SchemaVersion}
	out.Sites = append(out.Sites, base.Sites...)

	for _, e := range ext.Sites {
		idx := -1
		for i, b := range out.Sites {
			if b.Name == e.Name {
				idx = i
				break
			}
		}
		if idx < 0 {
			out.Sites = append(out.Sites, e)
			continue
		}
		out.Sites[idx] = mergeSite(out.Sites[idx], e)
	}
	return out
}

func mergeSite(b, e rawSite) rawSite {
	// Arrays are merged. Removal is applied in build(), NOT here: otherwise
	// a site newly defined by the external file would never have its own
	// domains_remove applied (that branch never enters merge).
	b.Domains = union(b.Domains, e.Domains)
	b.CDNPatterns = union(b.CDNPatterns, e.CDNPatterns)
	b.LegacyDomains = union(b.LegacyDomains, e.LegacyDomains)
	b.MatchPatterns = union(b.MatchPatterns, e.MatchPatterns)
	b.CanaryURLs = union(b.CanaryURLs, e.CanaryURLs)

	// Extra is overwritten per key: the external file writes only the key it
	// wants to change instead of rewriting the whole map.
	if len(e.Extra) > 0 {
		if b.Extra == nil {
			b.Extra = map[string]string{}
		}
		for k, v := range e.Extra {
			b.Extra[k] = v
		}
	}
	b.DomainsRemove = union(b.DomainsRemove, e.DomainsRemove)
	b.CDNPatternsRemove = union(b.CDNPatternsRemove, e.CDNPatternsRemove)

	// Scalars are overwritten, but only if given.
	if e.UserAgent != "" {
		b.UserAgent = e.UserAgent
	}
	if e.RefererPolicy != "" {
		b.RefererPolicy = e.RefererPolicy
	}
	if e.DNSResolver != "" {
		b.DNSResolver = e.DNSResolver
	}
	if e.MaxConcurrent != 0 {
		b.MaxConcurrent = e.MaxConcurrent
	}
	if e.MaxSegments != 0 {
		b.MaxSegments = e.MaxSegments
	}
	if e.MaxConnections != 0 {
		b.MaxConnections = e.MaxConnections
	}
	if e.MaxRetries != 0 {
		b.MaxRetries = e.MaxRetries
	}
	if e.BaseDelay != "" {
		b.BaseDelay = e.BaseDelay
	}
	if e.MaxDelay != "" {
		b.MaxDelay = e.MaxDelay
	}
	if e.MaxElapsed != "" {
		b.MaxElapsed = e.MaxElapsed
	}
	return b
}

// union merges preserving order and drops duplicates.
func union(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// remove applies a removal list.
//
// The comparison is NORMALIZED: the matching side (site.MatchHost) lowercases
// the host and drops a trailing dot. Comparing raw strings here would mean a
// user writing `domains_remove = ["PixelDrain.COM"]` silently removes nothing.
func remove(from, drop []string) []string {
	if len(drop) == 0 {
		return from
	}
	norm := func(s string) string {
		return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	}
	bad := make(map[string]bool, len(drop))
	for _, d := range drop {
		bad[norm(d)] = true
	}
	out := make([]string, 0, len(from))
	for _, s := range from {
		if !bad[norm(s)] {
			out = append(out, s)
		}
	}
	return out
}

func build(f file) ([]site.SiteConfig, error) {
	if len(f.Sites) == 0 {
		return nil, fmt.Errorf("%w: no site definitions", ErrUsage)
	}
	out := make([]site.SiteConfig, 0, len(f.Sites))
	seen := map[string]bool{}
	for _, r := range f.Sites {
		if r.Name == "" {
			return nil, fmt.Errorf("%w: site definition without a name", ErrUsage)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("%w: %q is defined twice", ErrUsage, r.Name)
		}
		seen[r.Name] = true

		switch r.RefererPolicy {
		case "", site.RefererNone, site.RefererItemPage, site.RefererOrigin:
		default:
			return nil, fmt.Errorf("%w: %s: invalid referer_policy %q (none|item_page|origin)",
				ErrUsage, r.Name, r.RefererPolicy)
		}
		// Removal is applied here: both merged sites and sites newly defined
		// by the external file go through the same path.
		domains := remove(r.Domains, r.DomainsRemove)
		cdn := remove(r.CDNPatterns, r.CDNPatternsRemove)

		// Every site needs domains, except the plain-file-link fallback: it
		// takes whatever no site recognizes.
		if r.Name != site.DirectName {
			if len(r.Domains) == 0 {
				return nil, fmt.Errorf("%w: %s: domains is empty", ErrUsage, r.Name)
			}
			if len(domains) == 0 {
				return nil, fmt.Errorf("%w: %s: domains_remove removed every domain", ErrUsage, r.Name)
			}
		}

		base, err := dur(r.Name, "base_delay", r.BaseDelay)
		if err != nil {
			return nil, err
		}
		maxD, err := dur(r.Name, "max_delay", r.MaxDelay)
		if err != nil {
			return nil, err
		}
		elapsed, err := dur(r.Name, "max_elapsed", r.MaxElapsed)
		if err != nil {
			return nil, err
		}

		out = append(out, site.SiteConfig{
			Name:           r.Name,
			Domains:        domains,
			LegacyDomains:  r.LegacyDomains,
			MatchPatterns:  r.MatchPatterns,
			CDNPatterns:    cdn,
			Extra:          r.Extra,
			CanaryURLs:     r.CanaryURLs,
			UserAgent:      r.UserAgent,
			RefererPolicy:  r.RefererPolicy,
			MaxConcurrent:  r.MaxConcurrent,
			MaxSegments:    r.MaxSegments,
			MaxConnections: r.MaxConnections,
			DNSResolver:    r.DNSResolver,
			MaxRetries:     r.MaxRetries,
			BaseDelay:      base,
			MaxDelay:       maxD,
			MaxElapsed:     elapsed,
		}.WithDefaults())
	}
	return out, nil
}

func dur(siteName, field, v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: could not parse %s=%q: %v", ErrUsage, siteName, field, v, err)
	}
	return d, nil
}
