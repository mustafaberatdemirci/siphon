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
	"strings"
	"sync"
)

// CyberfileName is the registry key.
const CyberfileName = "cyberfile"

// sites.toml [site.extra] keys for cyberfile.
const (
	// ExtraCyberfileDetails is the file details endpoint, the one the file
	// page's script asks for the download link.
	ExtraCyberfileDetails = "details_endpoint"
	// ExtraCyberfileList is the folder listing endpoint.
	ExtraCyberfileList = "list_endpoint"
)

const (
	cyberfileDefaultDetails = "https://cyberfile.me/account/ajax/file_details"
	cyberfileDefaultList    = "https://cyberfile.me/account/ajax/load_files"
	// cyberfilePerPage is how many entries a listing request asks for; the
	// site's own page asks for 30.
	cyberfilePerPageDefault = 100
	cyberfileMaxDepth       = 32
	cyberfileMaxPages       = 1000
)

var cyberfileExtra = map[string]bool{ExtraCyberfileDetails: true, ExtraCyberfileList: true}

// cyberfilePerPage is a variable so tests can make listings span pages.
var cyberfilePerPage = cyberfilePerPageDefault

// cyberfileReserved are the first path segments of the site's own pages: a
// link to one is not a file.
var cyberfileReserved = map[string]bool{
	"account": true, "api": true, "assets": true, "cache": true, "contact": true,
	"error": true, "faq": true, "js": true, "link_checker": true, "login": true,
	"plugins": true, "privacy": true, "register": true, "report_file": true,
	"rewards": true, "terms": true, "themes": true, "upload": true,
}

// NewCyberfile is the factory given to the registry.
//
// How cyberfile works (measured 2026-10-19): a file's page is
// cyberfile.me/<code> (also /<code>/<name> and on www.); it carries the
// file's numeric id in a showFileInformation(<id>) call. The page's script
// POSTs that id to /account/ajax/file_details, whose JSON holds the file's
// name and its Download button: a download server link
// (p2.cyberfile.me/<code>/<name>?download_token=<64 hex>). No account and no
// captcha. A made-up token is redirected to the site's error page, so a link
// is fetched fresh just before each attempt. The page gives the size only
// rounded ("2.17 GB"); the download server tells the real one.
//
// A folder link (cyberfile.me/folder/<hash>[/<name>], measured 2026-10-19)
// opens a page carrying the folder's numeric id in loadImages('folder',
// '<id>'); the page's script POSTs it to /account/ajax/load_files, whose
// JSON html lists one entry per file with its page (dtfullurl), name and
// exact size in bytes (dtsizeraw), page by page (pageStart from 1, perPage),
// with the number of pages in a hidden rspTotalPages field. No account is
// needed. Subfolder entries (folderIconLi, folderId) are what the site's
// script reads; NOT MEASURED, no folder with subfolders was at hand.
//
// A download (measured on a folder's file): the download servers take Range
// requests and send a strong ETag and Last-Modified, so a file resumes; they
// allow ONE open connection per IP, and a second one (another range, another
// file) gets 429. sites.toml keeps cyberfile to one file over one
// connection. A file whose data the storage server lost answers every valid
// token with a 404 naming the file (see ClassifyStatus).
func NewCyberfile(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	for k := range cfg.Extra {
		if !cyberfileExtra[k] {
			cfg.Logln("cyberfile: unrecognized extra key %q in config — ignored (possibly a typo)", k)
		}
	}
	return &cyberfile{
		cfg:     cfg,
		w:       web{cfg: cfg},
		details: cfg.ExtraOr(ExtraCyberfileDetails, cyberfileDefaultDetails),
		list:    cfg.ExtraOr(ExtraCyberfileList, cyberfileDefaultList),
		ids:     map[string]string{},
	}
}

type cyberfile struct {
	cfg     SiteConfig
	w       web
	details string
	list    string

	// ids: file code -> the site's numeric file id, so an attempt after the
	// first costs one request (the details), not two.
	mu  sync.Mutex
	ids map[string]string
}

type cfKind int

const (
	cfFile cfKind = iota
	cfFolder
)

func (c *cyberfile) Match(u string) bool {
	_, _, err := c.parse(u)
	return err == nil
}

// parse accepts a file page (cyberfile.me/<code>, /<code>/<name>), a folder
// (/folder/<hash>[/<name>]; the key returned is the hash) and a download
// server's link (p2.cyberfile.me/<code>/<name>?download_token=…), taken as
// its file: its token may have expired.
func (c *cyberfile) parse(raw string) (cfKind, string, error) {
	u, host, err := parseLink(raw)
	if err != nil {
		return 0, "", err
	}
	seg := splitPath(u.Path)
	switch {
	case MatchHost(host, c.cfg.Domains) || MatchHost(host, c.cfg.LegacyDomains) || MatchHost(host, c.cfg.MatchPatterns):
		if len(seg) >= 1 && seg[0] == "folder" {
			if len(seg) >= 2 && cfFolderKey(seg[1]) {
				return cfFolder, seg[1], nil
			}
			return 0, "", fmt.Errorf("unsupported folder link: %s", u.Path)
		}
		if (len(seg) == 1 || len(seg) == 2) && cfCode(seg[0]) && !cyberfileReserved[strings.ToLower(seg[0])] {
			return cfFile, seg[0], nil
		}
		return 0, "", fmt.Errorf("unsupported path: %s", u.Path)
	case MatchHost(host, c.cfg.CDNPatterns):
		if len(seg) == 2 && cfCode(seg[0]) {
			return cfFile, seg[0], nil
		}
		return 0, "", fmt.Errorf("unsupported download link: %s", u.Path)
	}
	return 0, "", fmt.Errorf("unknown host: %s", host)
}

// cfCode: file codes are short runs of letters and digits (four measured).
func cfCode(s string) bool {
	if len(s) < 2 || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// cfFolderKey: folder hashes are runs of hex digits (32 measured).
func cfFolderKey(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'f') && (r < 'A' || r > 'F') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// base is where pages are fetched: the first concrete domain.
func (c *cyberfile) base() string {
	for _, d := range c.cfg.Domains {
		if !strings.ContainsRune(d, '*') {
			return "https://" + normalizeHost(d)
		}
	}
	return "https://cyberfile.me"
}

// page is a file's canonical page: its identity in the ledger and the queue.
func (c *cyberfile) page(code string) string { return c.base() + "/" + code }

var (
	cfFileID   = regexp.MustCompile(`showFileInformation\((\d+)\)`)
	cfDownload = regexp.MustCompile(`openUrl\('([^']+)'`)
)

// fileID reads the numeric id from the file's page (once per code).
func (c *cyberfile) fileID(ctx context.Context, code string) (string, error) {
	c.mu.Lock()
	id, ok := c.ids[code]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	page := c.page(code)
	resp, body, err := c.w.get(ctx, page, nil)
	if err != nil {
		return "", err
	}
	c.cfg.Recordln("file.html", body)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", Errorf(LayerItemPage, page, "the file doesn't exist or was removed (HTTP 404)")
	case resp.StatusCode != http.StatusOK:
		return "", statusError(page, resp, body)
	}
	m := cfFileID.FindSubmatch(body)
	if m == nil {
		if hasCaptcha(string(body)) {
			return "", &LayerError{Layer: LayerChallenge, Evidence: page,
				Err: captchaError{errors.New("cyberfile asks for a captcha; open the page in a browser, then resume")}}
		}
		return "", Errorf(LayerParse, page, "no file id on the file page (did its layout change?)")
	}
	id = string(m[1])
	c.mu.Lock()
	c.ids[code] = id
	c.mu.Unlock()
	return id, nil
}

// fileDetails asks for the file's details: its name and a fresh download link.
func (c *cyberfile) fileDetails(ctx context.Context, code string) (name, link string, err error) {
	id, err := c.fileID(ctx, code)
	if err != nil {
		return "", "", err
	}
	page := c.page(code)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.details, strings.NewReader(url.Values{"u": {id}}.Encode()))
	if err != nil {
		return "", "", Errorf(LayerFetch, c.details, "could not build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", page)
	resp, body, err := c.w.do(req)
	if err != nil {
		return "", "", err
	}
	c.cfg.Recordln("file_details.json", body)
	switch {
	case resp.StatusCode == http.StatusInternalServerError && len(strings.TrimSpace(string(body))) == 0:
		// MEASURED: an id the site doesn't know gets an empty 500.
		return "", "", Errorf(LayerItemPage, page, "cyberfile has no details for this file (id %s); it may have been removed", id)
	case resp.StatusCode != http.StatusOK:
		return "", "", statusError(c.details, resp, body)
	}
	var d struct {
		Success   bool   `json:"success"`
		HTML      string `json:"html"`
		PageTitle string `json:"page_title"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return "", "", Errorf(LayerParse, c.details, "could not decode JSON: %v", err)
	}
	if !d.Success {
		return "", "", Errorf(LayerItemPage, page, "cyberfile refused the file's details")
	}
	name = strings.TrimSpace(html.UnescapeString(d.PageTitle))
	m := cfDownload.FindStringSubmatch(d.HTML)
	if m == nil {
		if hasCaptcha(d.HTML) {
			return name, "", &LayerError{Layer: LayerChallenge, Evidence: page,
				Err: captchaError{errors.New("cyberfile asks for a captcha; open the page in a browser, then resume")}}
		}
		return name, "", Errorf(LayerItemPage, page, "no Download button in the file's details: the uploader may not allow downloading it, or it needs an account")
	}
	link = html.UnescapeString(m[1])
	lu, err := url.Parse(link)
	if err != nil || (lu.Scheme != "https" && lu.Scheme != "http") || lu.Host == "" {
		return name, "", Errorf(LayerItemPage, page, "the Download button points to %q", link)
	}
	if !MatchHost(lu.Host, c.cfg.CDNPatterns) {
		c.cfg.Logln("cyberfile: download host %s is not in cdn_patterns (a signal, not a gate)", lu.Host)
	}
	return name, link, nil
}

// hasCaptcha reports a captcha widget on a page.
func hasCaptcha(page string) bool {
	low := strings.ToLower(page)
	return strings.Contains(low, "g-recaptcha") || strings.Contains(low, "cf-turnstile") || strings.Contains(low, "h-captcha")
}

func (c *cyberfile) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	kind, code, err := c.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}
	if kind == cfFolder {
		return c.resolveFolder(ctx, code, yield)
	}
	it, err := c.resolveFile(ctx, code)
	if err != nil {
		return nil, err
	}
	return nil, yield(it)
}

// resolveFile builds a file's Item. URL is the file's page: PrepareURL turns
// it into a fresh download link right before each attempt. The size is left
// unknown: the page only rounds it, and the download server tells it.
func (c *cyberfile) resolveFile(ctx context.Context, code string) (Item, error) {
	name, _, err := c.fileDetails(ctx, code)
	if err != nil {
		return Item{}, err
	}
	if name == "" {
		name = code
	}
	page := c.page(code)
	return Item{URL: page, SourcePage: page, Filename: name, Size: -1}, nil
}

func (c *cyberfile) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	kind, code, err := c.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if kind != cfFile {
		return Item{}, Errorf(LayerParse, sourcePage, "expected a file link, got a folder")
	}
	return c.resolveFile(ctx, code)
}

// PrepareURL turns a file's page into a fresh download link. Called before
// every attempt: how long a token lasts was not measured, and a made-up or
// stale one ends on the site's error page.
func (c *cyberfile) PrepareURL(ctx context.Context, rawURL string) (string, error) {
	kind, code, err := c.parse(rawURL)
	if err != nil || kind != cfFile {
		return "", Errorf(LayerParse, rawURL, "not a cyberfile file link")
	}
	_, link, err := c.fileDetails(ctx, code)
	return link, err
}

// ClassifyStatus explains the one failure that was measured: a valid
// download link the download server answers with 404 while naming the file
// in Content-Disposition (MEASURED 2026-10-19). The token was accepted and
// the file is known; its storage server can't find the data. Not retryable
// within minutes: cyberfile's own message for it is "File can not be located,
// please try again later". The download host isn't checked: cdn_patterns
// are a signal, not a gate, and the attachment header is what tells this
// 404 apart.
func (c *cyberfile) ClassifyStatus(resp *http.Response, body []byte) error {
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Disposition") == "" {
		return nil
	}
	evidence := ""
	if resp.Request != nil && resp.Request.URL != nil {
		evidence = resp.Request.URL.Host
	}
	return &LayerError{Layer: LayerCDN, Evidence: evidence,
		Err: errors.New("cyberfile accepted the download link, but its storage server can't find the file (HTTP 404); the file is broken on cyberfile's side, try again later")}
}

// ValidateResponse: a stale or made-up token is redirected to the site's
// error page, a web page from the site instead of the file. That is an
// expired link: the item is resolved again and gets a fresh one.
func (c *cyberfile) ValidateResponse(resp *http.Response) error {
	if !pageInsteadOfFile(resp, c.cfg.CDNPatterns) {
		return nil
	}
	evidence := ""
	if resp.Request != nil && resp.Request.URL != nil {
		evidence = resp.Request.URL.String()
	}
	return &LayerError{Layer: LayerCDN, Evidence: evidence,
		Err: fmt.Errorf("%w: cyberfile sent a web page instead of the file", ErrLinkExpired)}
}

// Diagnose follows a canary the way a download does: the file page (Fetch,
// Parse), the file details (ItemPage), the download host (CDN). Nothing is
// downloaded. With the site's root as the canary (the embedded default:
// files come and go, doctor must not hang on one) only the site answering is
// checked and the deeper layers are WARN; doctor -canary <file link> checks
// them all.
func (c *cyberfile) Diagnose(ctx context.Context) ([]LayerResult, error) {
	return diagnoseCanaries(ctx, c.cfg, func(ctx context.Context, canary string) ([]LayerResult, error) {
		kind, code, err := c.parse(canary)
		if err != nil || kind != cfFile {
			resp, body, gerr := c.w.get(ctx, canary, nil)
			if gerr != nil {
				return nil, gerr
			}
			c.cfg.Recordln("root.html", body)
			if resp.StatusCode != http.StatusOK {
				return nil, statusError(canary, resp, body)
			}
			return []LayerResult{
				{Layer: LayerFetch, Status: StatusOK, Detail: fmt.Sprintf("200, %d bytes", len(body))},
				{Layer: LayerParse, Status: StatusWarn, Detail: "no file canary, parsing not verified",
					Evidence: "run doctor with -canary <a cyberfile.me/<code> link>"},
				{Layer: LayerItemPage, Status: StatusWarn, Detail: "no file canary, file details not verified"},
				{Layer: LayerCDN, Status: StatusWarn, Detail: "no file canary, no download host seen"},
			}, nil
		}
		id, err := c.fileID(ctx, code)
		if err != nil {
			return nil, err
		}
		out := []LayerResult{
			{Layer: LayerFetch, Status: StatusOK, Detail: "file page answered"},
			{Layer: LayerParse, Status: StatusOK, Detail: "file id " + id},
		}
		name, link, err := c.fileDetails(ctx, code)
		if err != nil {
			return out, err
		}
		return append(out,
			LayerResult{Layer: LayerItemPage, Status: StatusOK, Detail: "file details gave a download link: " + name},
			cdnResult(c.cfg, link)), nil
	})
}
