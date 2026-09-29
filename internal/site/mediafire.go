package site

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// MediafireName is the registry key.
const MediafireName = "mediafire"

// ExtraMediafireAPI is the [site.extra] key for mediafire's API base.
const ExtraMediafireAPI = "api_endpoint"

const mediafireDefaultAPI = "https://www.mediafire.com/api/1.4"

var mediafireExtra = map[string]bool{ExtraMediafireAPI: true}

// NewMediafire is the factory given to the registry.
//
// How mediafire works (measured 2026-09-29): the public API
// (/api/1.4/file/get_info.php, folder/get_info.php, folder/get_content.php)
// answers without an account and gives each file's name, size and SHA-256
// (its "hash" field; checked against a downloaded file). The download link
// is not in the API: it is the "Download" button of the file's page, a
// download<N>.mediafire.com URL carrying a key, so it is read from the page
// just before each attempt (PrepareURL). The download servers take Range
// requests but send neither ETag nor Last-Modified; the SHA-256 is what lets
// a file use several connections and resume (dl.ValidatorSHA256).
func NewMediafire(cfg SiteConfig) Resolver {
	cfg = cfg.WithDefaults()
	for k := range cfg.Extra {
		if !mediafireExtra[k] {
			cfg.Logln("mediafire: unrecognized extra key %q in config — ignored (possibly a typo)", k)
		}
	}
	return &mediafire{
		cfg: cfg,
		w:   web{cfg: cfg},
		api: strings.TrimRight(cfg.ExtraOr(ExtraMediafireAPI, mediafireDefaultAPI), "/"),
	}
}

type mediafire struct {
	cfg SiteConfig
	w   web
	api string
}

// MediafireError is an error mediafire's API reported, e.g. 110 "Unknown or
// Invalid QuickKey", 112 "Unknown or invalid FolderKey", 293 "Folder blocked
// for DMCA violation".
type MediafireError struct {
	Code    string
	Message string
}

func (e *MediafireError) Error() string {
	return fmt.Sprintf("mediafire error %s: %s", e.Code, e.Message)
}

type mfKind int

const (
	mfFile mfKind = iota
	mfFolder
)

type mfRef struct {
	kind mfKind
	key  string
}

func (m *mediafire) Match(u string) bool {
	_, err := m.parse(u)
	return err == nil
}

// parse accepts the forms mediafire hands out:
//
//	www.mediafire.com/file/<quick key>[/<name>/file]   (also file_premium, download, view)
//	www.mediafire.com/?<quick key>, /download.php?<quick key>
//	www.mediafire.com/folder/<folder key>[/<name>]
//	download<N>.mediafire.com/<download key>/<quick key>/<name>
//
// A download server link is taken as its file: its key may have expired,
// and the file's page gives a fresh one.
func (m *mediafire) parse(raw string) (mfRef, error) {
	u, host, err := parseLink(raw)
	if err != nil {
		return mfRef{}, err
	}
	seg := splitPath(u.Path)
	if MatchHost(host, m.cfg.CDNPatterns) {
		if len(seg) >= 2 && mfKey(seg[1]) {
			return mfRef{kind: mfFile, key: seg[1]}, nil
		}
		return mfRef{}, fmt.Errorf("unsupported download link: %s", u.Path)
	}
	if !MatchHost(host, m.cfg.Domains) && !MatchHost(host, m.cfg.LegacyDomains) && !MatchHost(host, m.cfg.MatchPatterns) {
		return mfRef{}, fmt.Errorf("unknown host: %s", host)
	}
	if len(seg) == 0 || (len(seg) == 1 && seg[0] == "download.php") {
		if mfKey(u.RawQuery) {
			return mfRef{kind: mfFile, key: u.RawQuery}, nil
		}
		return mfRef{}, fmt.Errorf("no file key in %q", raw)
	}
	if len(seg) >= 2 && mfKey(seg[1]) {
		switch seg[0] {
		case "file", "file_premium", "download", "view":
			return mfRef{kind: mfFile, key: seg[1]}, nil
		case "folder":
			return mfRef{kind: mfFolder, key: seg[1]}, nil
		}
	}
	return mfRef{}, fmt.Errorf("unsupported path: %s", u.Path)
}

// mfKey: mediafire's keys are short runs of lowercase letters and digits
// (quick keys measured at 11-15 characters, folder keys at 13).
func mfKey(s string) bool {
	if len(s) < 5 || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// pageBase is where file pages are fetched: the first concrete domain.
func (m *mediafire) pageBase() string {
	for _, d := range m.cfg.Domains {
		if !strings.ContainsRune(d, '*') {
			return "https://" + normalizeHost(d)
		}
	}
	return "https://www.mediafire.com"
}

// filePage is a file's canonical page: its identity in the ledger and the
// queue, and where the download link is read from.
func (m *mediafire) filePage(key string) string {
	return m.pageBase() + "/file/" + key
}

type mfFileInfo struct {
	QuickKey          string `json:"quickkey"`
	Filename          string `json:"filename"`
	Size              string `json:"size"`
	Hash              string `json:"hash"`
	PasswordProtected string `json:"password_protected"`
}

type mfFolderInfo struct {
	FolderKey   string `json:"folderkey"`
	Name        string `json:"name"`
	FileCount   string `json:"file_count"`
	FolderCount string `json:"folder_count"`
}

type mfResponse struct {
	Result  string          `json:"result"`
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`

	FileInfo      *mfFileInfo   `json:"file_info"`
	FolderInfo    *mfFolderInfo `json:"folder_info"`
	FolderContent *struct {
		Files      []mfFileInfo   `json:"files"`
		Folders    []mfFolderInfo `json:"folders"`
		MoreChunks string         `json:"more_chunks"`
	} `json:"folder_content"`
}

// call asks the API; an answer other than "Success" is an error carrying
// mediafire's own code and message.
func (m *mediafire) call(ctx context.Context, path string, params url.Values) (mfResponse, []byte, error) {
	params.Set("response_format", "json")
	rawURL := m.api + "/" + path + "?" + params.Encode()
	resp, body, err := m.w.get(ctx, rawURL, nil)
	if err != nil {
		return mfResponse{}, body, err
	}
	var env struct {
		Response mfResponse `json:"response"`
	}
	if jerr := json.Unmarshal(body, &env); jerr != nil {
		if resp.StatusCode != http.StatusOK {
			return mfResponse{}, body, statusError(rawURL, resp, body)
		}
		return mfResponse{}, body, Errorf(LayerParse, rawURL, "could not decode JSON: %v", jerr)
	}
	r := env.Response
	if r.Result != "Success" {
		mfErr := &MediafireError{Code: strings.Trim(string(r.Error), `"`), Message: r.Message}
		if mfErr.Message == "" {
			mfErr.Message = "result " + r.Result
		}
		return r, body, &LayerError{Layer: LayerFetch, Err: mfErr, Evidence: rawURL}
	}
	return r, body, nil
}

func (m *mediafire) Resolve(ctx context.Context, u string, yield func(Item) error) ([]ItemError, error) {
	ref, err := m.parse(u)
	if err != nil {
		return nil, Errorf(LayerParse, u, "%v", err)
	}
	if ref.kind == mfFile {
		it, err := m.resolveFile(ctx, ref.key)
		if err != nil {
			return nil, err
		}
		return nil, yield(it)
	}
	return m.resolveFolder(ctx, ref.key, yield)
}

func (m *mediafire) resolveFile(ctx context.Context, key string) (Item, error) {
	r, _, err := m.call(ctx, "file/get_info.php", url.Values{"quick_key": {key}})
	if err != nil {
		return Item{}, err
	}
	if r.FileInfo == nil {
		return Item{}, Errorf(LayerParse, m.filePage(key), "the API answer has no file_info")
	}
	return m.item(*r.FileInfo, "", 0)
}

// item builds the Item of a file the API described. URL is the file's page:
// PrepareURL turns it into a download link right before each attempt.
func (m *mediafire) item(f mfFileInfo, dir string, index int) (Item, error) {
	page := m.filePage(f.QuickKey)
	if f.QuickKey == "" || f.Filename == "" {
		return Item{}, Errorf(LayerParse, page, "the API gave no key or name for a file")
	}
	if f.PasswordProtected == "yes" {
		return Item{}, Errorf(LayerItemPage, page, "the file is password-protected; Siphon can't download it")
	}
	size, err := strconv.ParseInt(f.Size, 10, 64)
	if err != nil || size <= 0 {
		size = -1
	}
	sum := strings.ToLower(f.Hash)
	if !isSHA256(sum) {
		sum = ""
	}
	return Item{
		URL:        page,
		SourcePage: page,
		Dir:        dir,
		Filename:   f.Filename,
		SHA256:     sum,
		Size:       size,
		Index:      index,
	}, nil
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// mfMaxDepth bounds how deep folders are followed.
const mfMaxDepth = 32

// mfWalk resolves a folder tree: its own files first, then each subfolder
// under a folder of the same name.
type mfWalk struct {
	m        *mediafire
	yield    func(Item) error
	seen     map[string]bool
	index    int
	itemErrs []ItemError
	// stopped: yield asked to stop. Its error ends the walk; it is not a
	// subfolder that failed.
	stopped bool
}

func (m *mediafire) resolveFolder(ctx context.Context, key string, yield func(Item) error) ([]ItemError, error) {
	r, _, err := m.call(ctx, "folder/get_info.php", url.Values{"folder_key": {key}})
	if err != nil {
		return nil, err
	}
	if r.FolderInfo == nil {
		return nil, Errorf(LayerParse, key, "the API answer has no folder_info")
	}
	w := &mfWalk{m: m, yield: yield, seen: map[string]bool{}}
	// The root is always listed, even when its counts say it is empty: a
	// folder taken down for DMCA shows 0 files, and only the listing says why.
	root := mfFolderInfo{FolderKey: key, Name: r.FolderInfo.Name}
	if err := w.folder(ctx, root, sanitizeDirLabel(r.FolderInfo.Name, key), 0); err != nil {
		return w.itemErrs, err
	}
	if w.index == 0 && len(w.itemErrs) == 0 {
		return nil, Errorf(LayerParse, m.pageBase()+"/folder/"+key, "the folder has no files")
	}
	return w.itemErrs, nil
}

// folder yields one folder's files and walks its subfolders. Counts of "0"
// (given for subfolders) skip a listing that would come back empty.
func (w *mfWalk) folder(ctx context.Context, f mfFolderInfo, dir string, depth int) error {
	if w.seen[f.FolderKey] {
		return nil
	}
	w.seen[f.FolderKey] = true
	if depth > mfMaxDepth {
		return Errorf(LayerParse, f.FolderKey, "folders nested deeper than %d levels", mfMaxDepth)
	}
	if f.FileCount != "0" {
		err := w.m.list(ctx, f.FolderKey, "files", func(r mfResponse) error {
			for _, file := range r.FolderContent.Files {
				it, ierr := w.m.item(file, dir, w.index)
				if ierr != nil {
					w.itemErrs = append(w.itemErrs, ItemError{URL: w.m.filePage(file.QuickKey), Err: ierr})
					continue
				}
				w.index++
				if err := w.yield(it); err != nil {
					w.stopped = true
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if f.FolderCount == "0" {
		return nil
	}
	var subs []mfFolderInfo
	err := w.m.list(ctx, f.FolderKey, "folders", func(r mfResponse) error {
		subs = append(subs, r.FolderContent.Folders...)
		return nil
	})
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if sub.FolderKey == "" {
			continue
		}
		serr := w.folder(ctx, sub, dir+"/"+sanitizeDirLabel(sub.Name, sub.FolderKey), depth+1)
		if serr == nil {
			continue
		}
		if w.stopped || ctx.Err() != nil {
			return serr
		}
		// One subfolder that can't be listed doesn't sink the rest.
		w.itemErrs = append(w.itemErrs, ItemError{URL: w.m.pageBase() + "/folder/" + sub.FolderKey, Err: serr})
	}
	return nil
}

// list pages through a folder's files or folders, 1,000 at a time (the
// largest chunk the API gives).
func (m *mediafire) list(ctx context.Context, key, contentType string, each func(mfResponse) error) error {
	for chunk := 1; ; chunk++ {
		r, _, err := m.call(ctx, "folder/get_content.php", url.Values{
			"folder_key":   {key},
			"content_type": {contentType},
			"chunk":        {strconv.Itoa(chunk)},
			"chunk_size":   {"1000"},
		})
		if err != nil {
			return err
		}
		if r.FolderContent == nil {
			return Errorf(LayerParse, key, "the API answer has no folder_content")
		}
		if err := each(r); err != nil {
			return err
		}
		if r.FolderContent.MoreChunks != "yes" {
			return nil
		}
	}
}

func (m *mediafire) ResolveOne(ctx context.Context, sourcePage string) (Item, error) {
	ref, err := m.parse(sourcePage)
	if err != nil {
		return Item{}, Errorf(LayerParse, sourcePage, "%v", err)
	}
	if ref.kind != mfFile {
		return Item{}, Errorf(LayerParse, sourcePage, "expected a file page, got a folder")
	}
	return m.resolveFile(ctx, ref.key)
}

// PrepareURL turns a file's page into its download link. Called before
// every attempt, so each one gets a fresh key.
func (m *mediafire) PrepareURL(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", Errorf(LayerParse, rawURL, "%v", err)
	}
	if MatchHost(u.Host, m.cfg.CDNPatterns) {
		return rawURL, nil
	}
	return m.downloadLink(ctx, rawURL)
}

// downloadLink reads the Download button of a file page.
func (m *mediafire) downloadLink(ctx context.Context, page string) (string, error) {
	resp, body, err := m.w.get(ctx, page, nil)
	if err != nil {
		return "", err
	}
	text := string(body)
	if resp.StatusCode == http.StatusNotFound || strings.Contains(text, "Something appears to be missing") {
		return "", Errorf(LayerItemPage, page, "the file was removed from mediafire")
	}
	if resp.StatusCode != http.StatusOK {
		return "", statusError(page, resp, body)
	}
	link := mfButtonLink(text)
	if link == "" {
		low := strings.ToLower(text)
		if strings.Contains(low, "g-recaptcha") || strings.Contains(low, "cf-turnstile") || strings.Contains(low, "h-captcha") {
			return "", &LayerError{Layer: LayerChallenge, Evidence: page,
				Err: captchaError{fmt.Errorf("mediafire asks for a captcha; open the page in a browser, then resume")}}
		}
		return "", Errorf(LayerItemPage, page, "no download button on the file page")
	}
	lu, err := url.Parse(link)
	if err != nil || (lu.Scheme != "https" && lu.Scheme != "http") || lu.Host == "" {
		return "", Errorf(LayerItemPage, page, "the download button points to %q", link)
	}
	if !MatchHost(lu.Host, m.cfg.CDNPatterns) {
		m.cfg.Logln("mediafire: download host %s is not in cdn_patterns (a signal, not a gate)", lu.Host)
	}
	return link, nil
}

var (
	mfButtonTag = regexp.MustCompile(`<a\b[^>]*\bid="downloadButton"[^>]*>`)
	mfAttr      = regexp.MustCompile(`([a-zA-Z-]+)="([^"]*)"`)
)

// mfButtonLink returns the link of the page's Download button: its href, or
// the base64 data-scrambled-url some pages carry instead.
func mfButtonLink(page string) string {
	tag := mfButtonTag.FindString(page)
	if tag == "" {
		return ""
	}
	attrs := map[string]string{}
	for _, a := range mfAttr.FindAllStringSubmatch(tag, -1) {
		attrs[strings.ToLower(a[1])] = html.UnescapeString(a[2])
	}
	if s := attrs["data-scrambled-url"]; s != "" {
		if b, err := base64.StdEncoding.DecodeString(s); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return strings.TrimSpace(attrs["href"])
}

// ValidateResponse refuses a web page where the file should be: the
// download key expired and the answer came from the site instead of a
// download server. The next attempt reads a fresh link.
func (m *mediafire) ValidateResponse(resp *http.Response) error {
	if !pageInsteadOfFile(resp, m.cfg.CDNPatterns) {
		return nil
	}
	evidence := ""
	if resp.Request != nil && resp.Request.URL != nil {
		evidence = resp.Request.URL.String()
	}
	return &LayerError{Layer: LayerCDN, Evidence: evidence,
		Err: transientError{fmt.Errorf("the download server sent a web page instead of the file")}}
}

// Diagnose follows a canary file the way a download does: the API (Fetch,
// Parse), the file page's Download button (ItemPage), the download host
// (CDN).
func (m *mediafire) Diagnose(ctx context.Context) ([]LayerResult, error) {
	return diagnoseCanaries(ctx, m.cfg, func(ctx context.Context, canary string) ([]LayerResult, error) {
		ref, err := m.parse(canary)
		if err != nil || ref.kind != mfFile {
			return nil, Errorf(LayerParse, canary, "the canary must be a mediafire file link")
		}
		r, raw, err := m.call(ctx, "file/get_info.php", url.Values{"quick_key": {ref.key}})
		m.cfg.Recordln("file_info.json", raw)
		if err != nil {
			return nil, err
		}
		out := []LayerResult{{Layer: LayerFetch, Status: StatusOK, Detail: "API answered"}}
		if r.FileInfo == nil || r.FileInfo.Filename == "" {
			return out, Errorf(LayerParse, canary, "the API answer has no file name")
		}
		out = append(out, LayerResult{Layer: LayerParse, Status: StatusOK, Detail: "file_info: " + r.FileInfo.Filename})
		link, err := m.downloadLink(ctx, m.filePage(ref.key))
		if err != nil {
			return out, err
		}
		out = append(out,
			LayerResult{Layer: LayerItemPage, Status: StatusOK, Detail: "download button found"},
			cdnResult(m.cfg, link))
		return out, nil
	})
}
