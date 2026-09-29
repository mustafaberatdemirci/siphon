package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// CyberdropName is the registry key.
const CyberdropName = "cyberdrop"

// ExtraCyberdropAPI is the [site.extra] key for cyberdrop's API base.
const ExtraCyberdropAPI = "api_endpoint"

const cyberdropDefaultAPI = "https://api.cyberdrop.cr/api"

var cyberdropExtra = map[string]bool{ExtraCyberdropAPI: true}

// NewCyberdrop is the factory given to the registry.
//
// How cyberdrop works (measured 2026-09-29): an album's no-JavaScript page
// (cyberdrop.cr/a/<id>?nojs) lists every file with its id, name and exact
// size, so an album costs one request. A file's download link comes from
// the API (api.cyberdrop.cr/api/file/auth/<id>): a download server URL with
// a token that lasts 24 hours, so it is asked for just before each attempt.
// The download servers take Range requests but send neither ETag nor
// Last-Modified and the API gives no hash: a file is fetched over one
// connection and can't be resumed safely.
func NewCyberdrop(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	for k := range cfg.Extra {
		if !cyberdropExtra[k] {
			cfg.Logln("cyberdrop: unrecognized extra key %q in config — ignored (possibly a typo)", k)
		}
	}
	api := strings.TrimRight(cfg.ExtraOr(ExtraCyberdropAPI, cyberdropDefaultAPI), "/")
	apiHost := ""
	if u, err := url.Parse(api); err == nil {
		apiHost = normalizeHost(u.Host)
	}
	return &cyberdrop{cfg: cfg, w: web{cfg: cfg}, api: api, apiHost: apiHost}
}

type cyberdrop struct {
	cfg     SiteConfig
	w       web
	api     string
	apiHost string
}

// CyberdropError is an error cyberdrop's API answered with, e.g. 404 "File
// not found".
type CyberdropError struct {
	Status  int
	Message string
}

func (e *CyberdropError) Error() string {
	return fmt.Sprintf("cyberdrop %d: %s", e.Status, e.Message)
}

// Retryable: the server's own trouble passes with time.
func (e *CyberdropError) Retryable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

type cdKind int

const (
	cdAlbum cdKind = iota
	cdFile
)

func (c *cyberdrop) Match(u string) bool {
	_, _, err := c.parse(u)
	return err == nil
}

// parse accepts cyberdrop.cr/a/<album>, /f/<file> and /e/<file> (the
// embed page), on the old domains too, and links to the API or a download
// server that name a file (.../api/file/{info,auth,d}/<file>), taken as that
// file: a download link's token may have expired.
func (c *cyberdrop) parse(raw string) (cdKind, string, error) {
	u, host, err := parseLink(raw)
	if err != nil {
		return 0, "", err
	}
	seg := splitPath(u.Path)
	if host == c.apiHost || MatchHost(host, c.cfg.CDNPatterns) {
		if len(seg) == 4 && seg[0] == "api" && seg[1] == "file" &&
			(seg[2] == "info" || seg[2] == "auth" || seg[2] == "d") && cdID(seg[3]) {
			return cdFile, seg[3], nil
		}
		return 0, "", fmt.Errorf("unsupported link: %s", u.Path)
	}
	if !MatchHost(host, c.cfg.Domains) && !MatchHost(host, c.cfg.LegacyDomains) && !MatchHost(host, c.cfg.MatchPatterns) {
		return 0, "", fmt.Errorf("unknown host: %s", host)
	}
	switch {
	case len(seg) >= 2 && seg[0] == "a" && cdID(seg[1]):
		return cdAlbum, seg[1], nil
	case len(seg) == 2 && (seg[0] == "f" || seg[0] == "e") && cdID(seg[1]):
		return cdFile, seg[1], nil
	}
	return 0, "", fmt.Errorf("unsupported path: %s", u.Path)
}

// cdID: album and file ids are short runs of letters and digits.
func cdID(s string) bool {
	if len(s) < 4 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// base is where pages are fetched: the first concrete domain, never an old one.
func (c *cyberdrop) base() string {
	for _, d := range c.cfg.Domains {
		if !strings.ContainsRune(d, '*') {
			return "https://" + normalizeHost(d)
		}
	}
	return "https://cyberdrop.cr"
}

func (c *cyberdrop) filePage(id string) string { return c.base() + "/f/" + id }

// apiJSON asks the API; an answer other than 200 is a CyberdropError with
// the message the API gave.
func (c *cyberdrop) apiJSON(ctx context.Context, path string, out any) ([]byte, error) {
	rawURL := c.api + "/" + path
	resp, body, err := c.w.get(ctx, rawURL, nil)
	if err != nil {
		return body, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return body, &LayerError{Layer: LayerFetch, Err: &CyberdropError{Status: resp.StatusCode, Message: e.Error}, Evidence: rawURL}
		}
		return body, statusError(rawURL, resp, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return body, Errorf(LayerParse, rawURL, "could not decode JSON: %v", err)
	}
	return body, nil
}

func (c *cyberdrop) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	kind, id, err := c.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}
	if kind == cdFile {
		it, err := c.resolveFile(ctx, id)
		if err != nil {
			return nil, err
		}
		return nil, yield(it)
	}
	return c.resolveAlbum(ctx, id, yield)
}

func (c *cyberdrop) resolveFile(ctx context.Context, id string) (Item, error) {
	var info struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		Slug string `json:"slug"`
	}
	if _, err := c.apiJSON(ctx, "file/info/"+url.PathEscape(id), &info); err != nil {
		return Item{}, err
	}
	if info.Name == "" {
		return Item{}, Errorf(LayerParse, c.filePage(id), "the API gave no file name")
	}
	size := info.Size
	if size <= 0 {
		size = -1
	}
	return Item{URL: c.filePage(id), SourcePage: c.filePage(id), Filename: info.Name, Size: size}, nil
}

var (
	cdTitleRe = regexp.MustCompile(`<h1[^>]*\bid="title"[^>]*\btitle="([^"]*)"`)
	cdCountRe = regexp.MustCompile(`\bid="count"[^>]*>\s*([0-9][0-9,]*)\s+files?\b`)
	cdFileRe  = regexp.MustCompile(`<a\b[^>]*\bid="file"[^>]*>`)
	cdSizeRe  = regexp.MustCompile(`class="[^"]*\bfile-size\b[^"]*">\s*([0-9]+)\s*B\s*<`)
	cdAttrRe  = regexp.MustCompile(`([a-zA-Z-]+)="([^"]*)"`)
)

type cdEntry struct {
	id   string
	name string
	size int64
}

// cdAlbumPage reads the no-JavaScript album page: the title, the number
// of files it claims (-1 if missing) and the files listed.
func cdAlbumPage(page string) (title string, count int, entries []cdEntry) {
	if m := cdTitleRe.FindStringSubmatch(page); m != nil {
		title = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	count = -1
	if m := cdCountRe.FindStringSubmatch(page); m != nil {
		count, _ = strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	}
	anchors := cdFileRe.FindAllStringIndex(page, -1)
	for i, a := range anchors {
		attrs := map[string]string{}
		for _, m := range cdAttrRe.FindAllStringSubmatch(page[a[0]:a[1]], -1) {
			attrs[strings.ToLower(m[1])] = html.UnescapeString(m[2])
		}
		e := cdEntry{size: -1}
		if id, ok := strings.CutPrefix(attrs["href"], "/f/"); ok && cdID(id) {
			e.id = id
		}
		e.name = strings.TrimSpace(attrs["title"])
		// The exact size sits after the link, before the next one.
		end := len(page)
		if i+1 < len(anchors) {
			end = anchors[i+1][0]
		}
		if m := cdSizeRe.FindStringSubmatch(page[a[1]:end]); m != nil {
			e.size, _ = strconv.ParseInt(m[1], 10, 64)
		}
		entries = append(entries, e)
	}
	return title, count, entries
}

func (c *cyberdrop) resolveAlbum(ctx context.Context, id string, yield func(Item) error) ([]ItemError, error) {
	pageURL := c.base() + "/a/" + id + "?nojs"
	resp, body, err := c.w.get(ctx, pageURL, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, Errorf(LayerFetch, pageURL, "the album doesn't exist (removed, or the link is wrong)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(pageURL, resp, body)
	}
	c.cfg.Recordln("album.html", body)
	title, count, entries := cdAlbumPage(string(body))
	if title == "" && count < 0 && len(entries) == 0 {
		return nil, Errorf(LayerParse, pageURL, "no album on the page (did its layout change?)")
	}
	if len(entries) == 0 {
		return nil, Errorf(LayerParse, pageURL, "the album has no files")
	}
	dir := sanitizeDirLabel(title, id)
	var itemErrs []ItemError
	// Fewer files listed than the album claims: say so instead of quietly
	// downloading fewer.
	if count >= 0 && count != len(entries) {
		itemErrs = append(itemErrs, ItemError{URL: pageURL,
			Err: Errorf(LayerParse, fmt.Sprintf("count=%d, listed=%d", count, len(entries)), "the album page listed a different number of files than it claims")})
	}
	index := 0
	for i, e := range entries {
		if e.id == "" || e.name == "" {
			itemErrs = append(itemErrs, ItemError{URL: pageURL, Err: Errorf(LayerParse, fmt.Sprintf("file %d", i+1), "a listed file has no id or name")})
			continue
		}
		it := Item{URL: c.filePage(e.id), SourcePage: c.filePage(e.id), Dir: dir, Filename: e.name, Size: e.size, Index: index}
		if it.Size <= 0 {
			it.Size = -1
		}
		index++
		if err := yield(it); err != nil {
			return itemErrs, err
		}
	}
	return itemErrs, nil
}

func (c *cyberdrop) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	kind, id, err := c.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if kind != cdFile {
		return Item{}, Errorf(LayerParse, sourcePage, "expected a file page, got an album")
	}
	return c.resolveFile(ctx, id)
}

// PrepareURL turns a file's page into a signed download link. Called
// before every attempt: the token lasts 24 hours, a queue can wait longer.
func (c *cyberdrop) PrepareURL(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", Errorf(LayerParse, rawURL, "%v", err)
	}
	if MatchHost(u.Host, c.cfg.CDNPatterns) {
		return rawURL, nil
	}
	kind, id, err := c.parse(rawURL)
	if err != nil || kind != cdFile {
		return "", Errorf(LayerParse, rawURL, "not a cyberdrop file link")
	}
	return c.signedLink(ctx, id)
}

func (c *cyberdrop) signedLink(ctx context.Context, id string) (string, error) {
	var auth struct {
		URL string `json:"url"`
	}
	if _, err := c.apiJSON(ctx, "file/auth/"+url.PathEscape(id), &auth); err != nil {
		return "", err
	}
	lu, err := url.Parse(auth.URL)
	if err != nil || (lu.Scheme != "https" && lu.Scheme != "http") || lu.Host == "" {
		return "", Errorf(LayerItemPage, c.filePage(id), "the API gave the download link %q", auth.URL)
	}
	if !MatchHost(lu.Host, c.cfg.CDNPatterns) {
		c.cfg.Logln("cyberdrop: download host %s is not in cdn_patterns (a signal, not a gate)", lu.Host)
	}
	return auth.URL, nil
}

// ValidateResponse refuses a web page that came from somewhere other than
// a download server where the file should be; the next attempt asks for a
// fresh link.
func (c *cyberdrop) ValidateResponse(resp *http.Response) error {
	if !pageInsteadOfFile(resp, c.cfg.CDNPatterns) {
		return nil
	}
	evidence := ""
	if resp.Request != nil && resp.Request.URL != nil {
		evidence = resp.Request.URL.String()
	}
	return &LayerError{Layer: LayerCDN, Evidence: evidence,
		Err: transientError{fmt.Errorf("a web page came back instead of the file")}}
}

// Diagnose reads a canary album page (Fetch, Parse), then asks the file API:
// for the album's first file a signed link (ItemPage, CDN); for an empty
// album a file that doesn't exist, whose JSON "not found" still shows the
// API answering.
func (c *cyberdrop) Diagnose(ctx context.Context) ([]LayerResult, error) {
	return diagnoseCanaries(ctx, c.cfg, func(ctx context.Context, canary string) ([]LayerResult, error) {
		kind, id, err := c.parse(canary)
		if err != nil || kind != cdAlbum {
			return nil, Errorf(LayerParse, canary, "the canary must be a cyberdrop album link")
		}
		pageURL := c.base() + "/a/" + id + "?nojs"
		resp, body, err := c.w.get(ctx, pageURL, nil)
		if err != nil {
			return nil, err
		}
		c.cfg.Recordln("album.html", body)
		if resp.StatusCode != http.StatusOK {
			return nil, statusError(pageURL, resp, body)
		}
		out := []LayerResult{{Layer: LayerFetch, Status: StatusOK, Detail: fmt.Sprintf("200, %d bytes", len(body))}}
		title, count, entries := cdAlbumPage(string(body))
		if title == "" && count < 0 {
			return out, Errorf(LayerParse, pageURL, "no album title or file count on the page (did its layout change?)")
		}
		out = append(out, LayerResult{Layer: LayerParse, Status: StatusOK, Detail: fmt.Sprintf("album %q, %d files listed", title, len(entries))})
		if len(entries) > 0 && entries[0].id != "" {
			link, err := c.signedLink(ctx, entries[0].id)
			if err != nil {
				return out, err
			}
			return append(out,
				LayerResult{Layer: LayerItemPage, Status: StatusOK, Detail: "file API gave a signed link"},
				cdnResult(c.cfg, link)), nil
		}
		var none struct{}
		_, err = c.apiJSON(ctx, "file/info/siphonDoctorCheck", &none)
		var cdErr *CyberdropError
		if err != nil && !(errors.As(err, &cdErr) && cdErr.Status == http.StatusNotFound) {
			return out, err
		}
		return append(out, LayerResult{Layer: LayerItemPage, Status: StatusOK,
			Detail: "file API answered (the canary album is empty, so no download host was checked)"}), nil
	})
}
