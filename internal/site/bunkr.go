package site

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// BunkrName is the registry key.
const BunkrName = "bunkr"

// Extra keys and their defaults.
//
// These are bunkr's most frequently changing parts. In 2025-02 the download
// URL was in the HTML's <source src>; in 2025-03 it became
// get.bunkrr.su/api/vs; then apidl.bunkr.ru/api/_001_v2; as of 2026-09-10
// dl.bunkr.cr/api/_001_v2 PLUS a mandatory signing service. All of them live
// in config so the next change doesn't require a code change.
const (
	ExtraAPIEndpoint  = "api_endpoint"
	ExtraFallbackAPI  = "fallback_api_endpoint"
	ExtraDLOrigin     = "dl_origin"
	ExtraXORPrefix    = "xor_key_prefix"
	ExtraSignEndpoint = "sign_endpoint"
	ExtraLegacyPrefix = "legacy_path_prefix"

	defaultBunkrAPIEndpoint = "https://dl.bunkr.cr/api/_001_v2"
	defaultBunkrDLOrigin    = "https://dl.bunkr.cr"
	defaultBunkrXORPrefix   = "SECRET_KEY_"

	// Fallback endpoint: used when the primary endpoint is UNREACHABLE. On
	// this network dl.bunkr.cr is hijacked by the ISP at the DNS level, while
	// apidl.bunkr.ru is open (2026-09-10 measurement).
	defaultBunkrFallbackAPI = "https://apidl.bunkr.ru/api/_001_v2"

	// Storage prefix added to the path given by the old endpoint. Details in applyLegacyPrefix.
	defaultBunkrLegacyPrefix = "/storage/media"

	// Signing service. The CDN rejects unsigned requests with 403 without
	// looking at the file: the response for an existing and a nonexistent
	// file is byte-for-byte identical. 2026-09-10 measurement.
	defaultBunkrSignEndpoint = "https://glb-apisign.cdn.cr/sign"
)

// NewBunkr is the factory given to the registry.
func NewBunkr(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	b := &bunkr{
		cfg:          cfg,
		apiEndpoint:  cfg.ExtraOr(ExtraAPIEndpoint, defaultBunkrAPIEndpoint),
		dlOrigin:     strings.TrimRight(cfg.ExtraOr(ExtraDLOrigin, defaultBunkrDLOrigin), "/"),
		xorPrefix:    cfg.ExtraOr(ExtraXORPrefix, defaultBunkrXORPrefix),
		signEndpoint: cfg.ExtraOr(ExtraSignEndpoint, defaultBunkrSignEndpoint),
		fallbackAPI:  cfg.ExtraOr(ExtraFallbackAPI, defaultBunkrFallbackAPI),
		legacyPrefix: strings.TrimRight(cfg.ExtraOr(ExtraLegacyPrefix, defaultBunkrLegacyPrefix), "/"),
		burned:       map[string]burnInfo{},
		now:          time.Now,
	}
	// An unrecognized extra key is WARNED about. ExtraOr silently falls back
	// to the in-code default for a missing key, so a typo like "sign_endpont"
	// would be ignored without any symptom: you'd believe the fix you wrote
	// into the config was applied.
	known := map[string]bool{
		ExtraAPIEndpoint: true, ExtraFallbackAPI: true, ExtraDLOrigin: true,
		ExtraXORPrefix: true, ExtraSignEndpoint: true, ExtraLegacyPrefix: true,
	}
	for k := range cfg.Extra {
		if !known[k] {
			cfg.Logln("bunkr: unrecognized extra key %q in config — ignored (possibly a typo)", k)
		}
	}

	// The rotation pool is built from CONCRETE domains; wildcard entries only
	// serve recognition and can't be picked at random.
	for _, d := range cfg.Domains {
		if !strings.ContainsRune(d, '*') {
			b.all = append(b.all, normalizeHost(d))
		}
	}
	return b
}

type bunkr struct {
	cfg          SiteConfig
	apiEndpoint  string
	dlOrigin     string
	xorPrefix    string
	signEndpoint string
	fallbackAPI  string
	legacyPrefix string

	// primaryDeadAt is when the primary API endpoint was found unreachable;
	// zero means the endpoint is healthy. STICKY but TIME-LIMITED: sticky,
	// because otherwise a separate timeout would be waited for every file of
	// an album; time-limited, because the cause may be temporary (VPN,
	// network) and the window stays open for days. After primaryRetryAfter
	// the primary is tried again.
	primaryDeadAt time.Time

	// Rotation state. NOT global (in gallery-dl it is a package-level set);
	// tying it to the resolver instance makes parallel tests possible.
	mu     sync.Mutex
	all    []string            // concrete domains, in config ORDER
	burned map[string]burnInfo // domain -> reason and duration of elimination

	// now is the clock for elimination durations; tests move it forward.
	now func() time.Time
}

// burnInfo records why and until when a domain is eliminated.
type burnInfo struct {
	reason string
	until  time.Time
}

// Elimination durations. Elimination used to be permanent for the process
// lifetime: with the window open, even the few seconds of DNS outage that
// switching VPN causes eliminated EVERY domain and bunkr stayed dead until
// the app was restarted.
//
// There are two classes because they mean different things: failing to
// connect (DNS, timeout, refused) can be a temporary state of the local
// network and should be retried soon; a Cloudflare challenge or a certificate
// error is a real BLOCK by the site or the ISP and shouldn't be retried for
// nothing every two minutes.
const (
	burnTTLNetwork    = 2 * time.Minute
	burnTTLBlock      = 30 * time.Minute
	primaryRetryAfter = 10 * time.Minute
)

// ---------- URL recognition ----------

type bunkrKind int

const (
	bunkrAlbum bunkrKind = iota
	bunkrMedia
)

type bunkrRef struct {
	kind bunkrKind
	id   string // album id or media slug
	seg  string // path prefix for media: f, v, i, d
	host string
}

func (b *bunkr) Match(u string) bool {
	_, err := b.parse(u)
	return err == nil
}

func (b *bunkr) parse(raw string) (bunkrRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return bunkrRef{}, errors.New("empty URL")
	}
	if !strings.Contains(raw, "//") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bunkrRef{}, fmt.Errorf("could not parse URL: %w", err)
	}
	host := normalizeHost(u.Host)
	host = strings.TrimPrefix(host, "app.")

	known := MatchHost(host, b.cfg.Domains) ||
		MatchHost(host, b.cfg.LegacyDomains) ||
		MatchHost(host, b.cfg.MatchPatterns)
	if !known {
		return bunkrRef{}, fmt.Errorf("unknown host: %s", host)
	}

	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(seg) < 2 || seg[1] == "" {
		return bunkrRef{}, errors.New("incomplete path")
	}
	switch seg[0] {
	case "a":
		return bunkrRef{kind: bunkrAlbum, id: seg[1], host: host}, nil
	case "f", "v", "i", "d":
		return bunkrRef{kind: bunkrMedia, id: seg[1], seg: seg[0], host: host}, nil
	default:
		return bunkrRef{}, fmt.Errorf("unsupported path: /%s", strings.Join(seg, "/"))
	}
}

// ---------- Resolver interface ----------

func (b *bunkr) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	ref, err := b.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}

	if ref.kind == bunkrMedia {
		item, err := b.resolveMedia(ctx, "/"+ref.seg+"/"+ref.id)
		if err != nil {
			return nil, err
		}
		if err := yield(item); err != nil {
			return nil, err
		}
		return nil, nil
	}

	// advanced=1 is MANDATORY: window.albumFiles only comes with this
	// parameter. The old way (scraping grid-images_box divs) missed everything
	// after 100 files; gallery-dl changed its data source for exactly this
	// reason on 2025-08-31. So it wasn't a pagination problem, it was a source
	// problem.
	page, root, err := b.fetchWithRotation(ctx, "/a/"+url.PathEscape(ref.id)+"?advanced=1", "")
	if err != nil {
		return nil, err
	}
	text := string(page)

	files, err := parseAlbumFiles(text)
	if err != nil {
		return nil, Errorf(LayerParse, root+"/a/"+ref.id, "%v", err)
	}

	dir := sanitizeDirLabel(ogTitle(text), ref.id)

	var itemErrs []ItemError
	for i, f := range files {
		fileURL, referer, ferr := b.resolveFileURL(ctx, f.ID)
		if ferr != nil {
			itemErrs = append(itemErrs, ItemError{
				URL: root + "/f/" + f.Slug,
				Err: ferr,
			})
			continue
		}
		item := Item{
			URL:        fileURL,
			SourcePage: root + "/f/" + f.Slug,
			Headers:    map[string]string{"Referer": referer},
			Dir:        dir,
			Filename:   f.Name,
			Size:       f.Size,
			Index:      i,
			// bunkr doesn't give sha256; resume relies on ETag/Last-Modified.
		}
		if err := yield(item); err != nil {
			return itemErrs, err
		}
	}
	return itemErrs, nil
}

// resolveMedia produces an Item from a single media page.
func (b *bunkr) resolveMedia(ctx context.Context, path string) (Item, error) {
	page, root, err := b.fetchWithRotation(ctx, path, "")
	if err != nil {
		return Item{}, err
	}
	text := string(page)

	dataID := extractBetween(text, `data-file-id="`, `"`)
	if dataID == "" {
		return Item{}, Errorf(LayerParse, root+path, "data-file-id not found")
	}
	fileURL, referer, err := b.resolveFileURL(ctx, dataID)
	if err != nil {
		return Item{}, err
	}

	name := strings.TrimSpace(ogTitle(text))
	if name == "" {
		name = strings.TrimPrefix(path[strings.LastIndex(path, "/"):], "/")
	}
	return Item{
		URL:        fileURL,
		SourcePage: root + path,
		Headers:    map[string]string{"Referer": referer},
		Filename:   name,
		Size:       -1,
		Index:      0,
	}, nil
}

// ResolveOne re-resolves a single item when the signed URL expired.
// On bunkr this path is vital: the XOR key is tied to an hourly window.
func (b *bunkr) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	ref, err := b.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if ref.kind != bunkrMedia {
		return Item{}, Errorf(LayerParse, sourcePage, "expected an item page, got an album")
	}
	return b.resolveMedia(ctx, "/"+ref.seg+"/"+ref.id)
}

// ClassifyStatus lets the downloader interpret 403/410 correctly.
func (b *bunkr) ClassifyStatus(resp *http.Response, body []byte) error {
	if resp.StatusCode != http.StatusForbidden {
		return nil
	}
	if looksLikeChallenge(resp, body) {
		evidence := ""
		if resp.Request != nil && resp.Request.URL != nil {
			evidence = resp.Request.URL.String()
		}
		return &LayerError{
			Layer:    LayerChallenge,
			Err:      errors.New("the CDN returned a Cloudflare challenge"),
			Evidence: evidence,
		}
	}
	// Not a challenge: the signed URL may have expired; let the downloader
	// re-resolve with ResolveOne.
	return nil
}

// maintenanceNames are the placeholder files bunkr serves in maintenance mode.
var maintenanceNames = []string{"/maint.mp4", "/maintenance-vid.mp4"}

// ValidateResponse catches the maintenance placeholder.
//
// For deleted or maintenance files bunkr returns a placeholder video with
// 200, NOT 404. The status is clean, the content is garbage. Without this
// check the tool counts the garbage as "downloaded successfully"; the worst
// kind of silent corruption. gallery-dl had to add the same detection in two
// separate commits.
func (b *bunkr) ValidateResponse(resp *http.Response) error {
	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.Path
	}
	for _, name := range maintenanceNames {
		if strings.HasSuffix(final, name) {
			return Errorf(LayerCDN, final,
				"the file server is in maintenance mode: a placeholder video came back, not the real content")
		}
	}
	return nil
}
