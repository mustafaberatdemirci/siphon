package site

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const gofileTestUA = "test-agent/1.0"

func gofileCfg() SiteConfig {
	return SiteConfig{
		Name:        GofileName,
		Domains:     []string{"gofile.io"},
		CDNPatterns: []string{"*.gofile.io"},
		UserAgent:   gofileTestUA,
		CanaryURLs:  []string{"https://gofile.io/d/canary"},
	}.WithDefaults()
}

// fakeGofile plays gofile's API (api.gofile.io) the way it was measured:
// guest accounts from POST /accounts, listings that need both the bearer
// token and the X-Website-Token, paging through metadata.hasNextPage.
type fakeGofile struct {
	now   time.Time
	nodes map[string]gofileNode // by id and by code
	// passwords holds the SHA-256 a protected folder wants.
	passwords map[string]string
	valid     map[string]bool
	pageSize  int

	accounts   int
	rateLimits int // answer this many listings with error-rateLimit first
	calls      []string
}

func newFakeGofile() *fakeGofile {
	return &fakeGofile{
		now:       time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
		nodes:     map[string]gofileNode{},
		passwords: map[string]string{},
		valid:     map[string]bool{},
		pageSize:  100,
	}
}

func yes() *bool { b := true; return &b }

func gfFile(id, name string, size, created int64) gofileNode {
	return gofileNode{ID: id, Type: "file", Name: name, Size: size, CreateTime: created, CanAccess: yes(),
		Link: "https://store1.gofile.io/download/web/" + id + "/" + name}
}

// addFolder stores a folder under its id and code; its children are the
// nodes given (their own children are not embedded, as in the real API).
func (f *fakeGofile) addFolder(id, code, name string, kids ...gofileNode) {
	n := gofileNode{ID: id, Code: code, Type: "folder", Name: name, CanAccess: yes(), Children: map[string]gofileNode{}}
	for _, k := range kids {
		shallow := k
		shallow.Children = nil
		n.Children[k.ID] = shallow
		if k.Type == "file" {
			f.nodes[k.ID] = k
		}
	}
	n.ChildrenCount = len(n.Children)
	f.nodes[id] = n
	if code != "" {
		f.nodes[code] = n
	}
}

func (f *fakeGofile) wantWebsiteToken(tok string) string {
	sum := sha256.Sum256([]byte(gofileTestUA + "::en-US::" + tok + "::" + strconv.FormatInt(f.now.Unix()/14400, 10) + "::" + gofileDefaultSalt))
	return hex.EncodeToString(sum[:])
}

func gfWrite(w http.ResponseWriter, v map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGofile) serve(w http.ResponseWriter, r *http.Request) {
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Method == http.MethodPost && r.URL.Path == "/accounts" {
		f.accounts++
		tok := "guest" + strconv.Itoa(f.accounts)
		f.valid[tok] = true
		gfWrite(w, map[string]any{"status": "ok", "data": map[string]any{"token": tok}})
		return
	}
	id, ok := strings.CutPrefix(r.URL.Path, "/contents/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case !f.valid[tok]:
		gfWrite(w, map[string]any{"status": "error-wrongToken", "data": map[string]any{}})
		return
	case r.Header.Get("X-Website-Token") != f.wantWebsiteToken(tok) || r.Header.Get("X-BL") != "en-US":
		gfWrite(w, map[string]any{"status": "error-notPremium", "data": map[string]any{}})
		return
	case f.rateLimits > 0:
		f.rateLimits--
		w.WriteHeader(http.StatusTooManyRequests)
		gfWrite(w, map[string]any{"status": "error-rateLimit", "data": map[string]any{}})
		return
	}
	n, ok := f.nodes[id]
	if !ok {
		gfWrite(w, map[string]any{"status": "error-notFound", "data": map[string]any{}})
		return
	}
	if want := f.passwords[n.ID]; want != "" {
		if got := r.URL.Query().Get("password"); got != want {
			status := "passwordRequired"
			if got != "" {
				status = "passwordWrong"
			}
			no := false
			gfWrite(w, map[string]any{"status": "ok", "data": gofileNode{ID: n.ID, Type: "folder", Name: n.Name,
				CanAccess: &no, Password: true, PasswordStatus: status}})
			return
		}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	keys := make([]string, 0, len(n.Children))
	for k := range n.Children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	start := min((page-1)*f.pageSize, len(keys))
	end := min(start+f.pageSize, len(keys))
	out := n
	if n.Type == "folder" {
		out.Children = map[string]gofileNode{}
		for _, k := range keys[start:end] {
			out.Children[k] = n.Children[k]
		}
	}
	gfWrite(w, map[string]any{"status": "ok", "data": out, "metadata": map[string]any{"hasNextPage": end < len(keys)}})
}

func newGofileWith(t *testing.T, f *fakeGofile, extra map[string]string) *gofile {
	t.Helper()
	cfg := gofileCfg()
	cfg.Extra = extra
	cfg.HTTPClient = &http.Client{Transport: &hostRouter{handlers: map[string]http.HandlerFunc{"api.gofile.io": f.serve}}}
	g := NewGofile(cfg).(*gofile)
	g.now = func() time.Time { return f.now }
	g.backoff = time.Millisecond
	return g
}

func (f *fakeGofile) count(call string) int {
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestGofileMatch(t *testing.T) {
	g := NewGofile(gofileCfg())
	for _, u := range []string{
		"https://gofile.io/d/PSHbQR",
		"gofile.io/d/PSHbQR",
		"https://gofile.io/d/5c3ebe49-13ef-4fc6-b773-bcdeab283598",
		"https://gofile.io/d/ABC654?password=1234",
		"https://store5.gofile.io/download/web/5c3ebe49-13ef-4fc6-b773-bcdeab283598/WinDeckOS%202.0.mrimg",
		"https://cold8.gofile.io/download/5c3ebe49-13ef-4fc6-b773-bcdeab283598/x.bin",
	} {
		if !g.Match(u) {
			t.Errorf("Match(%q) = false, want true", u)
		}
	}
	for _, u := range []string{
		"https://gofile.io/",
		"https://gofile.io/d/",
		"https://gofile.io/d/abc/def",
		"https://gofile.io/myProfile",
		"https://store5.gofile.io/download/direct/abc/x.bin",
		// SECURITY: only gofile's own hosts.
		"https://gofile.io.evil.test/d/PSHbQR",
		"https://evil.test/d/PSHbQR",
		"",
	} {
		if g.Match(u) {
			t.Errorf("Match(%q) = true, want false", u)
		}
	}
}

// A single upload (a folder named after its code, one file) lands in the
// output root, with the account token as the download cookie; the session's
// guest account is made once.
func TestGofileSingleUpload(t *testing.T) {
	f := newFakeGofile()
	f.addFolder("root-uuid", "AbCd12", "AbCd12", gfFile("file-uuid-1", "setup.zip", 1234, 100))
	g := newGofileWith(t, f, nil)

	items, _, err := collect(t, g, "https://gofile.io/d/AbCd12")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}
	it := items[0]
	if it.Dir != "" || it.Filename != "setup.zip" || it.Size != 1234 ||
		it.URL != "https://store1.gofile.io/download/web/file-uuid-1/setup.zip" ||
		it.SourcePage != "https://gofile.io/d/file-uuid-1" {
		t.Errorf("item = %+v", it)
	}
	if it.Headers["Cookie"] != "accountToken=guest1" {
		t.Errorf("headers = %v, want the guest token as the accountToken cookie", it.Headers)
	}

	// The same session reuses the account; re-resolving takes the file id.
	one, err := g.ResolveOne(context.Background(), it.SourcePage)
	if err != nil || one.URL != it.URL || one.Filename != it.Filename {
		t.Errorf("ResolveOne = %+v, %v", one, err)
	}
	// So does a download server link someone pasted.
	if _, _, err := collect(t, g, it.URL); err != nil {
		t.Errorf("download link: %v", err)
	}
	if n := f.count("POST /accounts"); n != 1 {
		t.Errorf("%d guest accounts made, want 1 for the session", n)
	}
}

func TestGofileFolderTree(t *testing.T) {
	f := newFakeGofile()
	f.pageSize = 2 // three top-level entries take two pages
	sub := gofileNode{ID: "sub-uuid", Code: "SubCd1", Type: "folder", Name: "Disc 1/2", CanAccess: yes(), CreateTime: 50}
	locked := gofileNode{ID: "locked-uuid", Code: "Lock01", Type: "folder", Name: "Locked", CanAccess: yes(), CreateTime: 60}
	f.addFolder("root-uuid", "Top123", "My Upload",
		gfFile("f-b", "b.txt", 2, 20), gfFile("f-a", "a.txt", 1, 10), sub, locked)
	f.addFolder("sub-uuid", "SubCd1", "Disc 1/2", gfFile("f-c", "c.txt", 3, 30))
	f.addFolder("locked-uuid", "Lock01", "Locked", gfFile("f-d", "d.txt", 4, 40))
	f.passwords["locked-uuid"] = "somehash"
	g := newGofileWith(t, f, nil)

	items, itemErrs, err := collect(t, g, "https://gofile.io/d/Top123")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, it := range items {
		got = append(got, it.Dir+"|"+it.Filename)
		if it.Index != i {
			t.Errorf("%s: index %d, want %d", it.Filename, it.Index, i)
		}
	}
	// Upload order, then subfolders; a "/" in a folder's name stays in it.
	want := []string{"My Upload|a.txt", "My Upload|b.txt", "My Upload/Disc 1-2|c.txt"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(itemErrs) != 1 || !strings.Contains(itemErrs[0].Err.Error(), "password") {
		t.Errorf("item errors = %v, want the locked folder reported", itemErrs)
	}
}

func TestGofilePasswordFolder(t *testing.T) {
	f := newFakeGofile()
	f.addFolder("root-uuid", "Priv01", "Private", gfFile("f-a", "a.txt", 1, 10), gfFile("f-b", "b.txt", 1, 11))
	sum := sha256.Sum256([]byte("hunter2"))
	f.passwords["root-uuid"] = hex.EncodeToString(sum[:])
	g := newGofileWith(t, f, nil)

	_, _, err := collect(t, g, "https://gofile.io/d/Priv01")
	if err == nil || !strings.Contains(err.Error(), "?password=") {
		t.Errorf("without a password: %v", err)
	}
	_, _, err = collect(t, g, "https://gofile.io/d/Priv01?password=wrong")
	if err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Errorf("wrong password: %v", err)
	}
	items, _, err := collect(t, g, "https://gofile.io/d/Priv01?password=hunter2")
	if err != nil || len(items) != 2 {
		t.Fatalf("right password: %d items, %v", len(items), err)
	}
	// The source page carries the hash, never the password itself.
	if strings.Contains(items[0].SourcePage, "hunter2") || !strings.Contains(items[0].SourcePage, f.passwords["root-uuid"]) {
		t.Errorf("source page %q", items[0].SourcePage)
	}
}

// A rate limit is waited out; a guest token the API stopped taking is
// replaced once.
func TestGofileRateLimitAndExpiredGuest(t *testing.T) {
	f := newFakeGofile()
	f.addFolder("root-uuid", "AbCd12", "AbCd12", gfFile("file-uuid-1", "setup.zip", 1, 1))
	f.rateLimits = 2
	g := newGofileWith(t, f, nil)
	if _, _, err := collect(t, g, "https://gofile.io/d/AbCd12"); err != nil {
		t.Fatalf("after two rate limits: %v", err)
	}

	f.valid = map[string]bool{} // the guest account is gone
	items, _, err := collect(t, g, "https://gofile.io/d/AbCd12")
	if err != nil {
		t.Fatalf("after the guest token expired: %v", err)
	}
	if items[0].Headers["Cookie"] != "accountToken=guest2" {
		t.Errorf("cookie %q, want the new guest token", items[0].Headers["Cookie"])
	}

	f.rateLimits = 10
	_, _, err = collect(t, g, "https://gofile.io/d/AbCd12")
	var rt interface{ Retryable() bool }
	if !errors.As(err, &rt) || !rt.Retryable() {
		t.Errorf("a lasting rate limit: %v, want a retryable error", err)
	}
}

func TestGofileOwnAccountToken(t *testing.T) {
	f := newFakeGofile()
	f.addFolder("root-uuid", "AbCd12", "AbCd12", gfFile("file-uuid-1", "setup.zip", 1, 1))
	f.valid["mine"] = true
	g := newGofileWith(t, f, map[string]string{ExtraGofileAccountToken: "mine"})
	items, _, err := collect(t, g, "https://gofile.io/d/AbCd12")
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Headers["Cookie"] != "accountToken=mine" || f.count("POST /accounts") != 0 {
		t.Errorf("cookie %q, %d guest accounts; want the configured token and none", items[0].Headers["Cookie"], f.count("POST /accounts"))
	}
}

func TestGofileErrors(t *testing.T) {
	f := newFakeGofile()
	g := newGofileWith(t, f, nil)
	_, _, err := collect(t, g, "https://gofile.io/d/Gone12")
	var gfErr *GofileError
	if !errors.As(err, &gfErr) || gfErr.Status != "error-notFound" {
		t.Errorf("missing content: %v", err)
	}

	// A salt gofile no longer uses: the API refuses every listing.
	f.addFolder("root-uuid", "AbCd12", "AbCd12", gfFile("file-uuid-1", "setup.zip", 1, 1))
	stale := newGofileWith(t, f, map[string]string{ExtraGofileSalt: "old-salt"})
	_, _, err = collect(t, stale, "https://gofile.io/d/AbCd12")
	if err == nil || !strings.Contains(err.Error(), "website_token_salt") {
		t.Errorf("stale salt: %v, want a pointer to website_token_salt", err)
	}
}

func TestGofileStopsWhenAsked(t *testing.T) {
	f := newFakeGofile()
	one := gofileNode{ID: "one-uuid", Code: "One111", Type: "folder", Name: "One", CanAccess: yes(), CreateTime: 1}
	two := gofileNode{ID: "two-uuid", Code: "Two222", Type: "folder", Name: "Two", CanAccess: yes(), CreateTime: 2}
	f.addFolder("root-uuid", "Top123", "Top", one, two)
	f.addFolder("one-uuid", "One111", "One", gfFile("f-x", "x.bin", 1, 1))
	f.addFolder("two-uuid", "Two222", "Two", gfFile("f-y", "y.bin", 1, 1))
	g := newGofileWith(t, f, nil)

	stop := errors.New("stop")
	yields := 0
	itemErrs, err := g.Resolve(context.Background(), "https://gofile.io/d/Top123", func(Item) error {
		yields++
		return stop
	})
	if !errors.Is(err, stop) || yields != 1 || len(itemErrs) != 0 {
		t.Errorf("Resolve = %v after %d yields, item errors %v", err, yields, itemErrs)
	}
	if f.count("GET /contents/Two222") != 0 {
		t.Error("the walk went on after the stop")
	}
}

func TestGofileWebPageMeansTheSessionExpired(t *testing.T) {
	g := NewGofile(gofileCfg()).(*gofile)
	page := &http.Response{Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}}
	if err := g.ValidateResponse(page); !errors.Is(err, ErrLinkExpired) {
		t.Errorf("a web page: %v, want ErrLinkExpired so the item is resolved again", err)
	}
	file := &http.Response{Header: http.Header{"Content-Type": {"application/octet-stream"}}}
	if err := g.ValidateResponse(file); err != nil {
		t.Errorf("a file was refused: %v", err)
	}
	// Measured: without a valid cookie the download server redirects to
	// gofile.io/d/<id>, so the page comes from the site's own host.
	site, _ := http.NewRequest(http.MethodGet, "https://gofile.io/d/5c3ebe49-13ef-4fc6-b773-bcdeab283598", nil)
	if err := g.ValidateResponse(&http.Response{Header: http.Header{"Content-Type": {"text/html"}}, Request: site}); !errors.Is(err, ErrLinkExpired) {
		t.Errorf("the redirect to the site: %v", err)
	}
	// An uploaded .html comes from the download server as a page: it is the file.
	upload, _ := http.NewRequest(http.MethodGet, "https://store1.gofile.io/download/web/id/index.html", nil)
	if err := g.ValidateResponse(&http.Response{Header: http.Header{"Content-Type": {"text/html"}}, Request: upload}); err != nil {
		t.Errorf("an uploaded .html was refused: %v", err)
	}
}

// The guest account outlives the run: the next run (a new resolver with the
// same state folder) uses it instead of making another, until it is a day
// old or the API stops taking it.
func TestGofileGuestAccountIsKeptForADay(t *testing.T) {
	f := newFakeGofile()
	f.addFolder("root-uuid", "AbCd12", "AbCd12", gfFile("file-uuid-1", "setup.zip", 1, 1))
	state := t.TempDir()
	run := func() string {
		t.Helper()
		g := newGofileWith(t, f, nil)
		g.cfg.StateDir = state
		items, _, err := collect(t, g, "https://gofile.io/d/AbCd12")
		if err != nil || len(items) != 1 {
			t.Fatalf("%d items, %v", len(items), err)
		}
		return items[0].Headers["Cookie"]
	}

	if got := run(); got != "accountToken=guest1" {
		t.Fatalf("first run: %q", got)
	}
	if got := run(); got != "accountToken=guest1" || f.accounts != 1 {
		t.Errorf("second run: %q with %d accounts made, want the saved one", got, f.accounts)
	}

	f.now = f.now.Add(25 * time.Hour)
	if got := run(); got != "accountToken=guest2" {
		t.Errorf("a day later: %q, want a new account", got)
	}

	f.valid = map[string]bool{} // gofile dropped the account early
	if got := run(); got != "accountToken=guest3" {
		t.Errorf("after the account was rejected: %q", got)
	}
	if got := run(); got != "accountToken=guest3" || f.accounts != 3 {
		t.Errorf("the replacement wasn't saved: %q with %d accounts made", got, f.accounts)
	}
}
