package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Fake cyberfile ---
//
// It answers in the shapes measured on 2026-10-19: a file page carrying
// showFileInformation(<id>), a details endpoint whose JSON holds the name
// and an openUrl('<download link>') button, a download server that sends a
// made-up token to the error page, and a file whose storage is missing
// (404 naming the file).

type cfFakeFile struct {
	id, name string
	body     []byte
	noButton bool // the details have no Download button
	missing  bool // valid tokens get 404 naming the file
}

type cfFake struct {
	mu      sync.Mutex
	files   map[string]cfFakeFile // code -> file
	tokens  map[string]string     // valid token -> code
	seq     int
	pages   int
	details int
	reject  int // the next this many valid tokens are refused as if expired

	folders map[string]string       // folder hash -> node id
	nodes   map[string]cfFakeFolder // node id -> folder
	lists   int                     // listing requests
}

type cfFakeFolder struct {
	title string
	files []string // file codes
	subs  []string // subfolder node ids
}

func newCfFake() *cfFake {
	return &cfFake{files: map[string]cfFakeFile{}, tokens: map[string]string{},
		folders: map[string]string{}, nodes: map[string]cfFakeFolder{}}
}

// listing answers /account/ajax/load_files the way the site does: one div
// per entry (subfolders first), the page count in a hidden field.
func (f *cfFake) listing(w http.ResponseWriter, r *http.Request) {
	f.lists++
	_ = r.ParseForm()
	folder, ok := f.nodes[r.PostForm.Get("nodeId")]
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var entries []string
	for _, id := range folder.subs {
		entries = append(entries, fmt.Sprintf(`<div dttitle="%s" folderId="%s" class="col-xs-4 folderIconLi">`, html.EscapeString(f.nodes[id].title), id))
	}
	for _, code := range folder.files {
		file := f.files[code]
		name := html.EscapeString(file.name)
		entries = append(entries, fmt.Sprintf(`<div dttitle="%s" dtsizeraw="%d" dtfullurl="https://cyberfile.me/%s" dtfilename="%s" title="%s (1 MB)" fileId="%s" class="col-xs-4 image-thumb fileIconLi">`,
			name, len(file.body), code, name, name, file.id))
	}
	per, _ := strconv.Atoi(r.PostForm.Get("perPage"))
	page, _ := strconv.Atoi(r.PostForm.Get("pageStart"))
	if per <= 0 {
		per = 30
	}
	pages := (len(entries) + per - 1) / per
	from, to := (page-1)*per, page*per
	if from > len(entries) {
		from = len(entries)
	}
	if to > len(entries) {
		to = len(entries)
	}
	body := strings.Join(entries[from:to], "</div>") + fmt.Sprintf(`<input id="rspTotalPages" value="%d" type="hidden"/>`, pages)
	_ = json.NewEncoder(w).Encode(map[string]any{"html": body, "page_title": folder.title, "page_url": "https://cyberfile.me/folder/x"})
}

func (f *cfFake) site(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/account/ajax/load_files" {
		f.listing(w, r)
		return
	}
	if seg := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); seg[0] == "folder" && len(seg) >= 2 {
		node, ok := f.folders[seg[1]]
		if !ok {
			// measured: an unknown folder is sent to the account page
			http.Redirect(w, r, "https://cyberfile.me/account", http.StatusFound)
			return
		}
		fmt.Fprintf(w, `<script>loadImages('folder', '%s', 1, 0, '', {'searchTerm': ""});</script>`, node)
		return
	}
	if r.URL.Path == "/account" {
		fmt.Fprint(w, `<html><title>Login - CyberFile</title></html>`)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/account/ajax/file_details" {
		f.details++
		_ = r.ParseForm()
		for code, file := range f.files {
			if file.id != r.PostForm.Get("u") {
				continue
			}
			f.seq++
			tok := fmt.Sprintf("%064d", f.seq)
			f.tokens[tok] = code
			body := `<div class="file-preview-wrapper">Filesize: 2.17 GB`
			if !file.noButton {
				body += `<button type="button" class="btn btn-info" onClick="openUrl('https://p2.cyberfile.me/` + code + `/` +
					url.PathEscape(file.name) + `?download_token=` + tok + `'); return false;">Download</button>`
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "html": body + "</div>",
				"page_title": file.name, "page_url": "https://cyberfile.me/" + code, "javascript": ""})
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // measured: an empty 500
		return
	}
	if r.URL.Path == "/error" {
		fmt.Fprint(w, `<html><body>File can not be located, please try again later.</body></html>`)
		return
	}
	code := strings.Split(strings.Trim(r.URL.Path, "/"), "/")[0]
	file, ok := f.files[code]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<html><title>CyberFile - Easily Share your Files</title>404 Oops - Page Not Found!</html>`)
		return
	}
	f.pages++
	fmt.Fprintf(w, `<html><head><meta property="og:title" content="%s - CyberFile - CyberFile"/></head>
<script>$(document).ready(function(){ showFileInformation(%s); });</script></html>`, file.name, file.id)
}

func (f *cfFake) storage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	code, ok := f.tokens[r.URL.Query().Get("download_token")]
	reject := ok && f.reject > 0
	if reject {
		f.reject--
	}
	file := f.files[code]
	f.mu.Unlock()
	if !ok || reject || !strings.HasPrefix(r.URL.Path, "/"+code+"/") {
		http.Redirect(w, r, "https://cyberfile.me/error?e=File+can+not+be+located%2C+please+try+again+later.", http.StatusFound)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+file.name+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	if file.missing {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<html>404 Oops - Page Not Found!</html>`)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(file.body))
}

func cyberfileCfg() SiteConfig {
	return SiteConfig{
		Name:        CyberfileName,
		Domains:     []string{"cyberfile.me", "www.cyberfile.me"},
		CDNPatterns: []string{"*.cyberfile.me"},
		CanaryURLs:  []string{"https://cyberfile.me/"},
		MaxRetries:  1,
	}.WithDefaults()
}

func newCyberfileWith(f *cfFake) *cyberfile {
	rt := &hostRouter{handlers: map[string]http.HandlerFunc{"cyberfile.me": f.site, "www.cyberfile.me": f.site, "p2.cyberfile.me": f.storage}}
	cfg := cyberfileCfg()
	cfg.HTTPClient = &http.Client{Transport: rt}
	return NewCyberfile(cfg).(*cyberfile)
}

// --- Tests ---

func TestCyberfileParse(t *testing.T) {
	c := newCyberfileWith(newCfFake())
	for in, want := range map[string]string{
		"https://cyberfile.me/AbCd":          "AbCd",
		"cyberfile.me/AbCd":                  "AbCd",
		"https://www.cyberfile.me/AbCd":      "AbCd",
		"https://cyberfile.me/AbCd/clip.mp4": "AbCd",
		"https://p2.cyberfile.me/AbCd/clip.mp4?download_token=" + strings.Repeat("a", 64): "AbCd",
	} {
		kind, code, err := c.parse(in)
		if err != nil || kind != cfFile || code != want {
			t.Errorf("parse(%q) = %v %q %v, want file %q", in, kind, code, err, want)
		}
	}
	for _, in := range []string{
		"https://cyberfile.me/folder/5fc1a3e52f1a7706fb8a4754239bd4aa/Some_Name_(2022)",
		"https://cyberfile.me/folder/5fc1a3e52f1a7706fb8a4754239bd4aa",
	} {
		if kind, key, err := c.parse(in); err != nil || kind != cfFolder || key != "5fc1a3e52f1a7706fb8a4754239bd4aa" {
			t.Errorf("parse(%q) = %v %q %v, want the folder", in, kind, key, err)
		}
	}
	for _, bad := range []string{
		"https://cyberfile.me/",
		"https://cyberfile.me/account/login",
		"https://cyberfile.me/faq",
		"https://cyberfile.me/folder/",
		"https://cyberfile.me/folder/not-a-hash",
		"https://cyberfile.me/a/b/c",
		"https://p2.cyberfile.me/AbCd",
		"https://cyberfile.example/AbCd",
		"https://cyberdrop.cr/f/AbCd",
	} {
		if c.Match(bad) {
			t.Errorf("%q was taken as a cyberfile file", bad)
		}
	}
}

// A file resolves to its page, its exact name and an unknown size (the page
// only rounds it); every attempt gets a fresh download link, and the page is
// read once.
func TestCyberfileResolveAndPrepare(t *testing.T) {
	f := newCfFake()
	f.files["AbCd"] = cfFakeFile{id: "123456", name: "Sample Clip.mp4", body: []byte("x")}
	c := newCyberfileWith(f)

	var items []Item
	if _, err := c.Resolve(context.Background(), "https://p2.cyberfile.me/AbCd/old.mp4?download_token=stale", func(it Item) error {
		items = append(items, it)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}
	it := items[0]
	if it.SourcePage != "https://cyberfile.me/AbCd" || it.URL != it.SourcePage || it.Filename != "Sample Clip.mp4" || it.Size != -1 {
		t.Fatalf("item = %+v", it)
	}
	a, err := c.PrepareURL(context.Background(), it.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.PrepareURL(context.Background(), it.URL)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.HasPrefix(a, "https://p2.cyberfile.me/AbCd/") || !strings.Contains(a, "download_token=") {
		t.Errorf("links %q and %q: want two fresh download links", a, b)
	}
	if f.pages != 1 || f.details != 3 {
		t.Errorf("page read %d times, details %d times; want 1 and 3", f.pages, f.details)
	}
}

func TestCyberfileErrors(t *testing.T) {
	f := newCfFake()
	f.files["NoBt"] = cfFakeFile{id: "1", name: "a.bin", noButton: true}
	c := newCyberfileWith(f)
	noop := func(Item) error { return nil }

	_, err := c.Resolve(context.Background(), "https://cyberfile.me/Gone", noop)
	if l, _ := LayerOf(err); l != LayerItemPage || !strings.Contains(err.Error(), "removed") {
		t.Errorf("missing file: %v", err)
	}
	_, err = c.Resolve(context.Background(), "https://cyberfile.me/NoBt", noop)
	if err == nil || !strings.Contains(err.Error(), "Download button") {
		t.Errorf("no Download button: %v", err)
	}
	_, err = c.Resolve(context.Background(), "https://cyberfile.me/folder/00000000000000000000000000000000/x", noop)
	if err == nil || !strings.Contains(err.Error(), "doesn't exist or isn't shared") {
		t.Errorf("an unknown folder: %v", err)
	}
}

// The measured failure: the download server takes the token and names the
// file, then answers 404. That is said plainly, and is not mistaken for an
// expired link.
func TestCyberfileStorage404IsExplained(t *testing.T) {
	c := newCyberfileWith(newCfFake())
	req := httptest.NewRequest(http.MethodGet, "https://p2.cyberfile.me/AbCd/a.mp4?download_token=x", nil)
	resp := &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{"Content-Disposition": {`attachment; filename="a.mp4"`}}, Request: req}
	err := c.ClassifyStatus(resp, nil)
	if err == nil || !strings.Contains(err.Error(), "storage server can't find the file") {
		t.Fatalf("ClassifyStatus = %v", err)
	}
	var rt interface{ Retryable() bool }
	if errors.As(err, &rt) && rt.Retryable() {
		t.Error("a missing file is retried")
	}
	resp.Header = http.Header{}
	if err := c.ClassifyStatus(resp, nil); err != nil {
		t.Errorf("a plain 404 got classified: %v", err)
	}
}

// A stale token ends on the site's error page: an expired link, resolved
// again. A page from the download server itself may be the file.
func TestCyberfileErrorPageIsAnExpiredLink(t *testing.T) {
	c := newCyberfileWith(newCfFake())
	page := &http.Response{Header: http.Header{"Content-Type": {"text/html; charset=UTF-8"}},
		Request: httptest.NewRequest(http.MethodGet, "https://cyberfile.me/error?e=x", nil)}
	if err := c.ValidateResponse(page); !errors.Is(err, ErrLinkExpired) {
		t.Errorf("the error page: %v", err)
	}
	page.Request = httptest.NewRequest(http.MethodGet, "https://p2.cyberfile.me/AbCd/page.html", nil)
	if err := c.ValidateResponse(page); err != nil {
		t.Errorf("an .html file from the download server: %v", err)
	}
}

func TestCyberfileDiagnose(t *testing.T) {
	f := newCfFake()
	f.files["AbCd"] = cfFakeFile{id: "123456", name: "Sample Clip.mp4"}
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			fmt.Fprint(w, "<html>CyberFile - Easily Share your Files</html>")
			return
		}
		f.site(w, r)
	})
	c := newCyberfileWith(f)
	// Diagnose's DNS step looks the canary's host up for real; the test's
	// canary is on a host that always resolves.
	c.cfg.HTTPClient.Transport.(*hostRouter).handlers["localhost"] = root

	c.cfg.CanaryURLs = []string{"https://localhost/"}
	res, err := c.Diagnose(context.Background())
	if err != nil || hasFail(res) {
		t.Fatalf("root canary: %v %+v", err, res)
	}
	if !hasStatus(res, LayerParse, StatusWarn) {
		t.Errorf("without a file canary Parse should be WARN: %+v", res)
	}
}

func hasStatus(res []LayerResult, l Layer, s LayerStatus) bool {
	for _, r := range res {
		if r.Layer == l && r.Status == s {
			return true
		}
	}
	return false
}

// A folder resolves to its files with their exact sizes, every page of the
// listing, and its subfolders under folders of their own; a subfolder that
// leads back up isn't walked twice. A file's page isn't read again before
// its download: the listing gave its id.
func TestCyberfileFolder(t *testing.T) {
	old := cyberfilePerPage
	cyberfilePerPage = 2
	t.Cleanup(func() { cyberfilePerPage = old })
	f := newCfFake()
	names := []string{"clip 1.mp4", "Tom & Jerry.mp4", "clip 3.mp4", "clip 4.mp4", "clip 5.mp4"}
	var codes []string
	for i, n := range names {
		code := fmt.Sprintf("AAA%d", i+1)
		codes = append(codes, code)
		f.files[code] = cfFakeFile{id: fmt.Sprint(11 + i), name: n, body: bytes.Repeat([]byte("x"), 100+i)}
	}
	f.files["SUB1"] = cfFakeFile{id: "21", name: "inner.mp4", body: []byte("inner")}
	f.nodes["207"] = cfFakeFolder{title: "Root Folder", files: codes, subs: []string{"208"}}
	f.nodes["208"] = cfFakeFolder{title: "Sub & Co", files: []string{"SUB1"}, subs: []string{"207"}}
	f.folders["5fc1a3e52f1a7706fb8a4754239bd4aa"] = "207"
	c := newCyberfileWith(f)

	var items []Item
	itemErrs, err := c.Resolve(context.Background(), "https://cyberfile.me/folder/5fc1a3e52f1a7706fb8a4754239bd4aa/Root_Folder", func(it Item) error {
		items = append(items, it)
		return nil
	})
	if err != nil || len(itemErrs) != 0 {
		t.Fatalf("Resolve: %v %v", err, itemErrs)
	}
	if len(items) != 6 {
		t.Fatalf("%d items, want 6: %+v", len(items), items)
	}
	for i, it := range items[:5] {
		if it.Dir != "Root Folder" || it.Filename != names[i] || it.Size != int64(100+i) || it.SourcePage != "https://cyberfile.me/"+codes[i] || it.Index != i {
			t.Errorf("item %d = %+v", i, it)
		}
	}
	if in := items[5]; in.Dir != "Root Folder/Sub & Co" || in.Filename != "inner.mp4" || in.Size != 5 {
		t.Errorf("the subfolder's file = %+v", in)
	}
	// the root's 6 entries over 3 pages of 2, and the subfolder's one page
	if f.lists != 4 {
		t.Errorf("%d listing requests, want 4", f.lists)
	}
	if _, err := c.PrepareURL(context.Background(), items[1].URL); err != nil {
		t.Fatal(err)
	}
	if f.pages != 0 {
		t.Errorf("a file page was read %d times; the listing gave its id", f.pages)
	}
}
