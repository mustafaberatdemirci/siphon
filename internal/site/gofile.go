package site

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GofileName is the registry key.
const GofileName = "gofile"

// sites.toml [site.extra] keys for gofile.
const (
	// ExtraGofileAPI is the API base.
	ExtraGofileAPI = "api_endpoint"
	// ExtraGofileAccountToken is a gofile account's API token (gofile.io >
	// My profile). Optional: without it a guest account is made for the
	// session. It is never logged or recorded.
	ExtraGofileAccountToken = "account_token"
	// ExtraGofileSalt is the salt of the X-Website-Token header gofile's
	// own page computes (its wt.obf.js). If gofile changes it, every
	// listing fails with error-notPremium until this is updated.
	ExtraGofileSalt = "website_token_salt"
)

const (
	gofileDefaultAPI  = "https://api.gofile.io"
	gofileDefaultSalt = "12af056dacea0b"
	gofileLang        = "en-US"
	gofileMaxDepth    = 32
)

var gofileExtra = map[string]bool{ExtraGofileAPI: true, ExtraGofileAccountToken: true, ExtraGofileSalt: true}

// NewGofile is the factory given to the registry.
//
// How gofile works (measured 2026-09-29): links are gofile.io/d/<code>, a
// folder (a single upload is a folder with one file). The API
// (api.gofile.io/contents/<code or id>) lists it only with an account
// token: a guest account costs one POST to /accounts, and gofile limits how
// often those can be made, so one is kept for a day in StateDir. Every API
// call also needs X-Website-Token, a SHA-256 over the User-Agent, the
// language, the token, the current 4-hour window and a salt from gofile's
// page script; without it the API answers error-notPremium. The download
// servers want the token as the accountToken cookie; without it they
// redirect to gofile's page. They take Range requests and send
// Last-Modified. The API gives an MD5 per file, not a SHA-256.
func NewGofile(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	for k := range cfg.Extra {
		if !gofileExtra[k] {
			cfg.Logln("gofile: unrecognized extra key %q in config — ignored (possibly a typo)", k)
		}
	}
	return &gofile{
		cfg:     cfg,
		w:       web{cfg: cfg},
		api:     strings.TrimRight(cfg.ExtraOr(ExtraGofileAPI, gofileDefaultAPI), "/"),
		salt:    cfg.ExtraOr(ExtraGofileSalt, gofileDefaultSalt),
		ownKey:  strings.TrimSpace(cfg.ExtraOr(ExtraGofileAccountToken, "")),
		now:     time.Now,
		backoff: 3 * time.Second,
	}
}

type gofile struct {
	cfg  SiteConfig
	w    web
	api  string
	salt string
	// ownKey is the user's own account token, if configured.
	ownKey string

	mu    sync.Mutex
	guest string // the session's guest token, made on first use

	now     func() time.Time
	backoff time.Duration // first wait after error-rateLimit
}

// GofileError is a status gofile's API answered with, such as
// "error-notFound", "error-notPremium" or "error-rateLimit".
type GofileError struct {
	Status string
}

func (e *GofileError) Error() string {
	switch e.Status {
	case "error-notFound":
		return "gofile: not found (removed, or the link is wrong)"
	case "error-notPremium":
		return "gofile refused the request (error-notPremium): its website token may have changed; see website_token_salt in sites.toml"
	case "error-rateLimit":
		return "gofile: too many requests (error-rateLimit); try again in a few minutes"
	case "error-wrongToken":
		return "gofile: the account token is not valid (error-wrongToken)"
	}
	return "gofile: " + e.Status
}

// Retryable: a rate limit passes with time.
func (e *GofileError) Retryable() bool { return e.Status == "error-rateLimit" }

func (g *gofile) Match(u string) bool {
	_, _, err := g.parse(u)
	return err == nil
}

// parse accepts gofile.io/d/<code or id>, with an optional ?password=
// for a protected folder, and a download server's link
// (store5.gofile.io/download/web/<file id>/<name>), taken as its file: the
// API answers a file id with the file. It returns the content id and the
// SHA-256 of the password (what the API takes), "" without one.
func (g *gofile) parse(raw string) (id, password string, err error) {
	u, host, err := parseLink(raw)
	if err != nil {
		return "", "", err
	}
	seg := splitPath(u.Path)
	if MatchHost(host, g.cfg.CDNPatterns) && len(seg) >= 3 && seg[0] == "download" {
		switch {
		case len(seg) == 4 && seg[1] == "web" && gofileID(seg[2]):
			return seg[2], "", nil
		case len(seg) == 3 && gofileID(seg[1]):
			return seg[1], "", nil
		}
		return "", "", fmt.Errorf("unsupported download link: %s", u.Path)
	}
	if !MatchHost(host, g.cfg.Domains) && !MatchHost(host, g.cfg.LegacyDomains) && !MatchHost(host, g.cfg.MatchPatterns) {
		return "", "", fmt.Errorf("unknown host: %s", host)
	}
	if len(seg) != 2 || seg[0] != "d" || !gofileID(seg[1]) {
		return "", "", fmt.Errorf("unsupported path: %s", u.Path)
	}
	password = u.Query().Get("password")
	if password != "" && !isSHA256(password) {
		sum := sha256.Sum256([]byte(password))
		password = hex.EncodeToString(sum[:])
	}
	return seg[1], password, nil
}

// gofileID: folder codes are short runs of letters and digits, ids are UUIDs.
func gofileID(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// page is the canonical page of a content id; with a password its hash
// travels along, so re-resolving a file in a protected folder still works.
func (g *gofile) page(id, password string) string {
	p := "https://" + g.pageHost() + "/d/" + id
	if password != "" {
		p += "?password=" + password
	}
	return p
}

func (g *gofile) pageHost() string {
	for _, d := range g.cfg.Domains {
		if !strings.ContainsRune(d, '*') {
			return normalizeHost(d)
		}
	}
	return "gofile.io"
}

// token returns the account token: the user's own, or a guest account,
// kept for a day in the state folder so that neither a later run nor a
// second copy of Siphon has to make another (gofile rate-limits that).
func (g *gofile) token(ctx context.Context) (string, error) {
	if g.ownKey != "" {
		return g.ownKey, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.guest != "" {
		return g.guest, nil
	}
	if tok := g.loadGuest(); tok != "" {
		g.guest = tok
		return g.guest, nil
	}
	rawURL := g.api + "/accounts"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader("{}"))
	if err != nil {
		return "", Errorf(LayerFetch, rawURL, "could not build request: %v", err)
	}
	g.siteHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, body, err := g.w.do(req)
	if err != nil {
		return "", err
	}
	var env struct {
		Status string `json:"status"`
		Data   struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if jerr := json.Unmarshal(body, &env); jerr != nil {
		if resp.StatusCode != http.StatusOK {
			return "", statusError(rawURL, resp, body)
		}
		return "", Errorf(LayerParse, rawURL, "could not decode JSON: %v", jerr)
	}
	if env.Status != "ok" || env.Data.Token == "" {
		return "", &LayerError{Layer: LayerFetch, Err: &GofileError{Status: env.Status}, Evidence: rawURL}
	}
	g.guest = env.Data.Token
	g.saveGuest(g.guest)
	g.cfg.Logln("gofile: made a guest account")
	return g.guest, nil
}

// dropGuest forgets a guest token the API no longer accepts, on disk too.
func (g *gofile) dropGuest(tok string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.guest == tok {
		g.guest = ""
	}
	if g.loadGuest() == tok {
		_ = os.Remove(g.guestPath())
	}
}

// gofileGuestTTL is how long a saved guest account is used. The API
// rejecting it earlier makes a new one anyway; cyberdrop-dl keeps its guest
// account for the same day.
const gofileGuestTTL = 24 * time.Hour

type gofileGuestFile struct {
	Token string    `json:"token"`
	Made  time.Time `json:"made"`
}

func (g *gofile) guestPath() string {
	if g.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(g.cfg.StateDir, "gofile-guest.json")
}

// loadGuest returns the saved guest token while it is fresh, "" otherwise.
func (g *gofile) loadGuest() string {
	p := g.guestPath()
	if p == "" {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var f gofileGuestFile
	if json.Unmarshal(data, &f) != nil || f.Token == "" {
		return ""
	}
	if age := g.now().Sub(f.Made); age < 0 || age > gofileGuestTTL {
		return ""
	}
	return f.Token
}

// saveGuest writes the guest token for the next run. Failing to is not an
// error: the next run makes another account.
func (g *gofile) saveGuest(tok string) {
	p := g.guestPath()
	if p == "" {
		return
	}
	data, err := json.Marshal(gofileGuestFile{Token: tok, Made: g.now()})
	if err != nil || os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	if os.Rename(tmp, p) != nil {
		_ = os.Remove(tmp)
	}
}

// siteHeaders are what gofile's own page sends.
func (g *gofile) siteHeaders(req *http.Request) {
	req.Header.Set("Origin", "https://"+g.pageHost())
	req.Header.Set("Referer", "https://"+g.pageHost()+"/")
}

// websiteToken is the X-Website-Token gofile's page computes: SHA-256 of
// "UA::lang::token::<unix time / 4 h>::salt".
func (g *gofile) websiteToken(tok string) string {
	window := g.now().Unix() / 14400
	sum := sha256.Sum256([]byte(g.cfg.UserAgent + "::" + gofileLang + "::" + tok + "::" + strconv.FormatInt(window, 10) + "::" + g.salt))
	return hex.EncodeToString(sum[:])
}

type gofileNode struct {
	ID             string                `json:"id"`
	Type           string                `json:"type"`
	Name           string                `json:"name"`
	Code           string                `json:"code"`
	CanAccess      *bool                 `json:"canAccess"`
	Password       bool                  `json:"password"`
	PasswordStatus string                `json:"passwordStatus"`
	CreateTime     int64                 `json:"createTime"`
	Size           int64                 `json:"size"`
	Link           string                `json:"link"`
	MD5            string                `json:"md5"`
	ChildrenCount  int                   `json:"childrenCount"`
	Children       map[string]gofileNode `json:"children"`
}

// contents asks for one page of a content id. A rate limit is waited out a
// few times; a guest token the API rejects is replaced once.
func (g *gofile) contents(ctx context.Context, id, password string, page int) (gofileNode, bool, error) {
	q := url.Values{
		"page":          {strconv.Itoa(page)},
		"pageSize":      {"100"},
		"sortField":     {"createTime"},
		"sortDirection": {"1"},
	}
	if password != "" {
		q.Set("password", password)
	}
	rawURL := g.api + "/contents/" + url.PathEscape(id) + "?" + q.Encode()
	wait := g.backoff
	renewed := false
	for attempt := 0; ; attempt++ {
		tok, err := g.token(ctx)
		if err != nil {
			return gofileNode{}, false, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return gofileNode{}, false, Errorf(LayerFetch, rawURL, "could not build request: %v", err)
		}
		g.siteHeaders(req)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-BL", gofileLang)
		req.Header.Set("X-Website-Token", g.websiteToken(tok))
		resp, body, err := g.w.do(req)
		if err != nil {
			return gofileNode{}, false, err
		}
		var env struct {
			Status   string     `json:"status"`
			Data     gofileNode `json:"data"`
			Metadata struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"metadata"`
		}
		if jerr := json.Unmarshal(body, &env); jerr != nil {
			if resp.StatusCode != http.StatusOK {
				return gofileNode{}, false, statusError(rawURL, resp, body)
			}
			return gofileNode{}, false, Errorf(LayerParse, rawURL, "could not decode JSON: %v", jerr)
		}
		switch {
		case env.Status == "ok":
			return env.Data, env.Metadata.HasNextPage, nil
		case env.Status == "error-rateLimit" && attempt < 3:
			g.cfg.Logln("gofile: rate limited, waiting %s", wait)
			select {
			case <-ctx.Done():
				return gofileNode{}, false, ctx.Err()
			case <-time.After(wait):
			}
			wait *= 2
			continue
		case env.Status == "error-wrongToken" && g.ownKey == "" && !renewed:
			renewed = true
			g.dropGuest(tok)
			continue
		}
		return gofileNode{}, false, &LayerError{Layer: LayerFetch, Err: &GofileError{Status: env.Status}, Evidence: rawURL}
	}
}

// node reads a content id whole: every page, children merged.
func (g *gofile) node(ctx context.Context, id, password string) (gofileNode, error) {
	var out gofileNode
	for page := 1; ; page++ {
		n, more, err := g.contents(ctx, id, password, page)
		if err != nil {
			return gofileNode{}, err
		}
		if page == 1 {
			out = n
			out.Children = map[string]gofileNode{}
		}
		for k, c := range n.Children {
			out.Children[k] = c
		}
		if !more || page >= 1000 {
			return out, nil
		}
	}
}

// gofileAccessError explains why a node can't be read.
func gofileAccessError(n gofileNode, page string) error {
	if n.CanAccess == nil || *n.CanAccess {
		return nil
	}
	switch {
	case n.Password && n.PasswordStatus == "passwordWrong":
		return Errorf(LayerItemPage, page, "wrong password for this gofile folder")
	case n.Password:
		return Errorf(LayerItemPage, page, "the gofile folder is password-protected; add ?password=... to the link")
	}
	return Errorf(LayerItemPage, page, "the gofile folder is private")
}

func (g *gofile) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	id, password, err := g.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}
	root, err := g.node(ctx, id, password)
	if err != nil {
		return nil, err
	}
	if err := gofileAccessError(root, g.page(id, password)); err != nil {
		return nil, err
	}
	if root.Type == "file" {
		it, err := g.item(ctx, root, "", 0, password)
		if err != nil {
			return nil, err
		}
		return nil, yield(it)
	}
	// A single upload is a folder named after its own code holding one
	// file: that file goes to the output root, as a file link's would.
	dir := ""
	if !(len(root.Children) == 1 && (root.Name == root.Code || root.Name == "") && onlyFiles(root)) {
		dir = sanitizeDirLabel(root.Name, id)
		if strings.EqualFold(root.Name, "root") {
			dir = id
		}
	}
	w := &gofileWalk{g: g, yield: yield, password: password, seen: map[string]bool{root.ID: true}}
	if err := w.children(ctx, root, dir, 0); err != nil {
		return w.itemErrs, err
	}
	if w.index == 0 && len(w.itemErrs) == 0 {
		return nil, Errorf(LayerParse, g.page(id, password), "the gofile folder has no files")
	}
	return w.itemErrs, nil
}

func onlyFiles(n gofileNode) bool {
	for _, c := range n.Children {
		if c.Type != "file" {
			return false
		}
	}
	return true
}

type gofileWalk struct {
	g        *gofile
	yield    func(Item) error
	password string
	seen     map[string]bool
	index    int
	itemErrs []ItemError
	// stopped: yield asked to stop. Its error ends the walk; it is not a
	// subfolder that failed.
	stopped bool
}

// children yields a folder's files and walks its subfolders, in upload
// order (the API's children are a map; its order is lost in decoding).
func (w *gofileWalk) children(ctx context.Context, n gofileNode, dir string, depth int) error {
	kids := make([]gofileNode, 0, len(n.Children))
	for _, c := range n.Children {
		kids = append(kids, c)
	}
	sort.SliceStable(kids, func(i, j int) bool {
		if kids[i].CreateTime != kids[j].CreateTime {
			return kids[i].CreateTime < kids[j].CreateTime
		}
		return kids[i].Name < kids[j].Name
	})
	var folders []gofileNode
	for _, c := range kids {
		switch c.Type {
		case "file":
			it, err := w.g.item(ctx, c, dir, w.index, w.password)
			if err != nil {
				w.itemErrs = append(w.itemErrs, ItemError{URL: w.g.page(c.ID, w.password), Err: err})
				continue
			}
			w.index++
			if err := w.yield(it); err != nil {
				w.stopped = true
				return err
			}
		case "folder":
			folders = append(folders, c)
		}
	}
	for _, f := range folders {
		ref := f.Code
		if ref == "" {
			ref = f.ID
		}
		if ref == "" || w.seen[f.ID] {
			continue
		}
		w.seen[f.ID] = true
		err := w.folder(ctx, ref, dir+"/"+sanitizeDirLabel(f.Name, ref), depth+1)
		if err == nil {
			continue
		}
		if w.stopped || ctx.Err() != nil {
			return err
		}
		// One subfolder that can't be read doesn't sink the rest.
		w.itemErrs = append(w.itemErrs, ItemError{URL: w.g.page(ref, w.password), Err: err})
	}
	return nil
}

func (w *gofileWalk) folder(ctx context.Context, ref, dir string, depth int) error {
	if depth > gofileMaxDepth {
		return Errorf(LayerParse, ref, "folders nested deeper than %d levels", gofileMaxDepth)
	}
	n, err := w.g.node(ctx, ref, w.password)
	if err != nil {
		return err
	}
	if err := gofileAccessError(n, w.g.page(ref, w.password)); err != nil {
		return err
	}
	return w.children(ctx, n, dir, depth)
}

// item builds the Item of a file node. The link needs the account token as
// a cookie; the token is taken now, and a later re-resolution takes a fresh
// one if it expired.
func (g *gofile) item(ctx context.Context, n gofileNode, dir string, index int, password string) (Item, error) {
	page := g.page(n.ID, password)
	if n.ID == "" || n.Name == "" {
		return Item{}, Errorf(LayerParse, page, "the API gave no id or name for a file")
	}
	if err := gofileAccessError(n, page); err != nil {
		return Item{}, err
	}
	if n.Link == "" || n.Link == "overloaded" {
		return Item{}, &LayerError{Layer: LayerCDN, Evidence: page,
			Err: transientError{errors.New("gofile gave no download link (its servers are overloaded); try again later")}}
	}
	lu, err := url.Parse(n.Link)
	if err != nil || (lu.Scheme != "https" && lu.Scheme != "http") || lu.Host == "" {
		return Item{}, Errorf(LayerParse, page, "the download link %q is not a web address", n.Link)
	}
	if !MatchHost(lu.Host, g.cfg.CDNPatterns) {
		g.cfg.Logln("gofile: download host %s is not in cdn_patterns (a signal, not a gate)", lu.Host)
	}
	tok, err := g.token(ctx)
	if err != nil {
		return Item{}, err
	}
	size := n.Size
	if size <= 0 {
		size = -1
	}
	return Item{
		URL:        n.Link,
		SourcePage: page,
		Dir:        dir,
		Filename:   n.Name,
		Size:       size,
		Index:      index,
		Headers:    map[string]string{"Cookie": "accountToken=" + tok},
	}, nil
}

// ResolveOne re-reads a file by its id: the API answers a file id with the
// file itself.
func (g *gofile) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	id, password, err := g.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	n, err := g.node(ctx, id, password)
	if err != nil {
		return Item{}, err
	}
	if n.Type != "file" {
		return Item{}, Errorf(LayerParse, sourcePage, "expected a file, got a %s", n.Type)
	}
	return g.item(ctx, n, "", 0, password)
}

// ValidateResponse: a download server that doesn't take the token
// redirects to gofile's page. That is an expired session, not a file: the
// item is resolved again, with a fresh token if the API rejects the old one.
func (g *gofile) ValidateResponse(resp *http.Response) error {
	if !pageInsteadOfFile(resp, g.cfg.CDNPatterns) {
		return nil
	}
	evidence := ""
	if resp.Request != nil && resp.Request.URL != nil {
		evidence = resp.Request.URL.String()
	}
	return &LayerError{Layer: LayerCDN, Evidence: evidence,
		Err: fmt.Errorf("%w: the download server sent gofile's web page instead of the file", ErrLinkExpired)}
}

// Diagnose follows a canary link: the account token and the listing
// (Fetch, Parse), then the first file's download host (CDN). Nothing is
// downloaded.
func (g *gofile) Diagnose(ctx context.Context) ([]LayerResult, error) {
	return diagnoseCanaries(ctx, g.cfg, func(ctx context.Context, canary string) ([]LayerResult, error) {
		id, password, err := g.parse(canary)
		if err != nil {
			return nil, Errorf(LayerParse, canary, "the canary must be a gofile.io/d/... link")
		}
		n, err := g.node(ctx, id, password)
		if err != nil {
			return nil, err
		}
		out := []LayerResult{{Layer: LayerFetch, Status: StatusOK, Detail: "API answered with the account token"}}
		file, ok := n, n.Type == "file"
		for _, c := range n.Children {
			if !ok && c.Type == "file" {
				file, ok = c, true
			}
		}
		if !ok {
			return out, Errorf(LayerParse, canary, "the canary folder has no file")
		}
		out = append(out,
			LayerResult{Layer: LayerParse, Status: StatusOK, Detail: "listing decoded: " + file.Name},
			LayerResult{Layer: LayerItemPage, Status: StatusOK, Detail: "gofile has no separate file page; the listing gives the link"},
			cdnResult(g.cfg, file.Link))
		return out, nil
	})
}
