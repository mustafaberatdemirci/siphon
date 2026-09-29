package site

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func mfCfg() SiteConfig {
	return SiteConfig{
		Name:        MediafireName,
		Domains:     []string{"www.mediafire.com", "mediafire.com"},
		CDNPatterns: []string{"download*.mediafire.com"},
		CanaryURLs:  []string{"https://www.mediafire.com/file/canary12345/c.bin/file"},
	}.WithDefaults()
}

// fakeMediafire plays mediafire's API, its file pages and a download server
// (download7.mediafire.com), all through a hostRouter.
type fakeMediafire struct {
	files   map[string]mfFileInfo
	folders map[string]fakeMFFolder
	// pageSize is how many entries one get_content chunk carries.
	pageSize int
	// page overrides the file page of a quick key.
	page map[string]string
	// calls lists the API calls as "action?params".
	calls []string
}

type fakeMFFolder struct {
	name    string
	files   []string // quick keys
	folders []string // folder keys
	dmca    bool
}

func newFakeMediafire() *fakeMediafire {
	return &fakeMediafire{files: map[string]mfFileInfo{}, folders: map[string]fakeMFFolder{}, pageSize: 1000, page: map[string]string{}}
}

func (f *fakeMediafire) addFile(key, name, content string) {
	sum := sha256.Sum256([]byte(content))
	f.files[key] = mfFileInfo{QuickKey: key, Filename: name, Size: strconv.Itoa(len(content)),
		Hash: strings.ToUpper(hex.EncodeToString(sum[:])), PasswordProtected: "no"}
}

func (f *fakeMediafire) router() *hostRouter {
	return &hostRouter{handlers: map[string]http.HandlerFunc{
		"www.mediafire.com": f.serve,
	}}
}

func mfWrite(w http.ResponseWriter, status int, resp map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"response": resp})
}

func mfFail(w http.ResponseWriter, code int, msg string) {
	mfWrite(w, http.StatusNotFound, map[string]any{"result": "Error", "error": code, "message": msg})
}

func (f *fakeMediafire) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if strings.HasPrefix(r.URL.Path, "/api/1.4/") {
		action := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/1.4/"), ".php")
		q.Del("response_format")
		f.calls = append(f.calls, action+"?"+q.Encode())
	}
	switch r.URL.Path {
	case "/api/1.4/file/get_info.php":
		fi, ok := f.files[q.Get("quick_key")]
		if !ok {
			mfFail(w, 110, "Unknown or Invalid QuickKey")
			return
		}
		mfWrite(w, http.StatusOK, map[string]any{"result": "Success", "file_info": fi})
	case "/api/1.4/folder/get_info.php":
		fo, ok := f.folders[q.Get("folder_key")]
		if !ok {
			mfFail(w, 112, "Unknown or invalid FolderKey")
			return
		}
		files, folders := len(fo.files), len(fo.folders)
		if fo.dmca {
			files, folders = 0, 0
		}
		mfWrite(w, http.StatusOK, map[string]any{"result": "Success", "folder_info": map[string]any{
			"folderkey": q.Get("folder_key"), "name": fo.name,
			"file_count": strconv.Itoa(files), "folder_count": strconv.Itoa(folders)}})
	case "/api/1.4/folder/get_content.php":
		fo, ok := f.folders[q.Get("folder_key")]
		if !ok {
			mfFail(w, 112, "Unknown or invalid FolderKey")
			return
		}
		if fo.dmca {
			mfFail(w, 293, "Folder blocked for DMCA violation")
			return
		}
		chunk, _ := strconv.Atoi(q.Get("chunk"))
		var entries []any
		switch q.Get("content_type") {
		case "files":
			for _, k := range fo.files {
				entries = append(entries, f.files[k])
			}
		case "folders":
			for _, k := range fo.folders {
				sub := f.folders[k]
				entries = append(entries, map[string]any{"folderkey": k, "name": sub.name,
					"file_count": strconv.Itoa(len(sub.files)), "folder_count": strconv.Itoa(len(sub.folders))})
			}
		}
		start := min((chunk-1)*f.pageSize, len(entries))
		end := min(start+f.pageSize, len(entries))
		more := "no"
		if end < len(entries) {
			more = "yes"
		}
		content := map[string]any{"more_chunks": more}
		content[q.Get("content_type")] = append([]any{}, entries[start:end]...)
		mfWrite(w, http.StatusOK, map[string]any{"result": "Success", "folder_content": content})
	default:
		key, ok := strings.CutPrefix(r.URL.Path, "/file/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		key, _, _ = strings.Cut(key, "/")
		if page, ok := f.page[key]; ok {
			_, _ = w.Write([]byte(page))
			return
		}
		fi, ok := f.files[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<html><a class="input popsok"
           aria-label="Download file"
           href="https://download7.mediafire.com/dkey&amp;x/%s/%s"           id="downloadButton"
           rel="nofollow">Download</a></html>`, fi.QuickKey, fi.Filename)
	}
}

func newMediafire(t *testing.T, f *fakeMediafire) *mediafire {
	t.Helper()
	cfg := mfCfg()
	cfg.HTTPClient = &http.Client{Transport: f.router()}
	return NewMediafire(cfg).(*mediafire)
}

func collect(t *testing.T, r Resolver, u string) ([]Item, []ItemError, error) {
	t.Helper()
	var items []Item
	errs, err := r.Resolve(context.Background(), u, func(it Item) error {
		items = append(items, it)
		return nil
	})
	return items, errs, err
}

func TestMediafireMatch(t *testing.T) {
	m := NewMediafire(mfCfg())
	for _, u := range []string{
		"https://www.mediafire.com/file/ctppmpm7giofsgv/ADOFAI.vpk/file",
		"https://www.mediafire.com/file/ctppmpm7giofsgv",
		"https://mediafire.com/file_premium/ctppmpm7giofsgv/x",
		"https://www.mediafire.com/download/ctppmpm7giofsgv",
		"https://www.mediafire.com/view/ctppmpm7giofsgv/pic.jpg/file",
		"https://www.mediafire.com/?511jd1358yxvf26",
		"https://www.mediafire.com/download.php?511jd1358yxvf26",
		"https://www.mediafire.com/folder/ixh40veo6hrc5/kcc_samples",
		"www.mediafire.com/folder/ixh40veo6hrc5",
		"https://download1638.mediafire.com/tp4fcfaqi6pg/ctppmpm7giofsgv/ADOFAI.vpk",
	} {
		if !m.Match(u) {
			t.Errorf("Match(%q) = false, want true", u)
		}
	}
	for _, u := range []string{
		"https://www.mediafire.com/",
		"https://www.mediafire.com/?sharekey=abc",
		"https://www.mediafire.com/file/",
		"https://www.mediafire.com/file/NOT_A_KEY!",
		"https://www.mediafire.com/about/ctppmpm7giofsgv",
		"https://download1638.mediafire.com/only-one-segment",
		// SECURITY: only mediafire's own hosts.
		"https://mediafire.com.evil.test/file/ctppmpm7giofsgv",
		"https://pixeldrain.com/u/abc",
		"",
	} {
		if m.Match(u) {
			t.Errorf("Match(%q) = true, want false", u)
		}
	}
}

func TestMediafireFileResolvesFromTheAPIAndReadsTheButton(t *testing.T) {
	f := newFakeMediafire()
	f.addFile("abcde12345", "report.pdf", "pdf-content")
	m := newMediafire(t, f)

	items, _, err := collect(t, m, "https://www.mediafire.com/file/abcde12345/report.pdf/file")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("%d items, want 1", len(items))
	}
	it := items[0]
	sum := sha256.Sum256([]byte("pdf-content"))
	if it.Filename != "report.pdf" || it.Size != int64(len("pdf-content")) || it.Dir != "" ||
		it.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("item = %+v", it)
	}
	if it.SourcePage != "https://www.mediafire.com/file/abcde12345" || it.URL != it.SourcePage {
		t.Errorf("URL %q / SourcePage %q, want the canonical file page for both", it.URL, it.SourcePage)
	}

	// The download link is read from the page just before the download,
	// with its HTML entities decoded.
	link, err := m.PrepareURL(context.Background(), it.URL)
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://download7.mediafire.com/dkey&x/abcde12345/report.pdf" {
		t.Errorf("link = %q", link)
	}
	// A download server link is already one.
	if again, _ := m.PrepareURL(context.Background(), link); again != link {
		t.Errorf("PrepareURL changed a download link: %q", again)
	}

	// Re-resolution from the source page gives the same item.
	one, err := m.ResolveOne(context.Background(), it.SourcePage)
	if err != nil || one.URL != it.URL || one.SHA256 != it.SHA256 {
		t.Errorf("ResolveOne = %+v, %v", one, err)
	}
}

func TestMediafireScrambledButton(t *testing.T) {
	f := newFakeMediafire()
	f.addFile("abcde12345", "a.zip", "zip")
	link := "https://download9.mediafire.com/k/abcde12345/a.zip"
	f.page["abcde12345"] = `<a class="input popsok" aria-label="Download file" href="javascript:void(0)" ` +
		`data-scrambled-url="` + base64.StdEncoding.EncodeToString([]byte(link)) + `" id="downloadButton">x</a>`
	m := newMediafire(t, f)
	got, err := m.PrepareURL(context.Background(), m.filePage("abcde12345"))
	if err != nil || got != link {
		t.Errorf("PrepareURL = %q, %v; want %q", got, err, link)
	}
}

func TestMediafirePageProblems(t *testing.T) {
	f := newFakeMediafire()
	for _, k := range []string{"removed1234", "captcha1234", "nobutton123", "badlink1234"} {
		f.addFile(k, k+".bin", k)
	}
	f.page["removed1234"] = `<h3>Something appears to be missing...</h3>`
	f.page["captcha1234"] = `<div class="g-recaptcha" data-sitekey="x"></div>`
	f.page["nobutton123"] = `<html>a redesigned page</html>`
	f.page["badlink1234"] = `<a href="javascript:void(0)" id="downloadButton">x</a>`
	m := newMediafire(t, f)
	ctx := context.Background()

	_, err := m.PrepareURL(ctx, m.filePage("removed1234"))
	if err == nil || !strings.Contains(err.Error(), "removed") {
		t.Errorf("removed file: %v", err)
	}

	_, err = m.PrepareURL(ctx, m.filePage("captcha1234"))
	var captcha interface{ CaptchaRequired() bool }
	if !errors.As(err, &captcha) || !captcha.CaptchaRequired() {
		t.Errorf("captcha page: %v, want an error that asks for the user", err)
	}
	if l, _ := LayerOf(err); l != LayerChallenge {
		t.Errorf("captcha page layer = %q", l)
	}

	_, err = m.PrepareURL(ctx, m.filePage("nobutton123"))
	if l, _ := LayerOf(err); err == nil || l != LayerItemPage {
		t.Errorf("page without a button: %v (layer %q)", err, l)
	}
	_, err = m.PrepareURL(ctx, m.filePage("badlink1234"))
	if err == nil {
		t.Error("a non-http button link was accepted")
	}
}

func TestMediafireAPIErrors(t *testing.T) {
	f := newFakeMediafire()
	f.addFile("locked12345", "secret.zip", "x")
	fi := f.files["locked12345"]
	fi.PasswordProtected = "yes"
	f.files["locked12345"] = fi
	f.folders["dmcafolder1"] = fakeMFFolder{name: "Gone", files: []string{"locked12345"}, dmca: true}
	m := newMediafire(t, f)

	_, _, err := collect(t, m, "https://www.mediafire.com/file/zzzzzzzzzzzzzzz/x/file")
	var mfErr *MediafireError
	if !errors.As(err, &mfErr) || mfErr.Code != "110" {
		t.Errorf("unknown key: %v, want mediafire error 110", err)
	}
	_, _, err = collect(t, m, "https://www.mediafire.com/folder/zzzzzzzzzzzzz")
	if !errors.As(err, &mfErr) || mfErr.Code != "112" {
		t.Errorf("unknown folder: %v, want mediafire error 112", err)
	}
	_, _, err = collect(t, m, "https://www.mediafire.com/file/locked12345")
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("password-protected file: %v", err)
	}
	// Its counts say empty; only the listing says why.
	_, _, err = collect(t, m, "https://www.mediafire.com/folder/dmcafolder1")
	if err == nil || !strings.Contains(err.Error(), "DMCA") {
		t.Errorf("DMCA folder: %v, want mediafire's reason", err)
	}
}

func TestMediafireFolderTree(t *testing.T) {
	f := newFakeMediafire()
	f.pageSize = 2 // three root files take two chunks
	f.addFile("rootfile001", "a.txt", "a")
	f.addFile("rootfile002", "b.txt", "b")
	f.addFile("rootfile003", "c.txt", "c")
	f.addFile("subfile0001", "d.txt", "d")
	f.addFile("deepfile001", "e.txt", "e")
	f.addFile("locked00001", "f.txt", "f")
	fi := f.files["locked00001"]
	fi.PasswordProtected = "yes"
	f.files["locked00001"] = fi
	f.folders["rootfolder1"] = fakeMFFolder{name: "Data Set", files: []string{"rootfile001", "rootfile002", "rootfile003"},
		folders: []string{"subfolder01", "emptyfolder", "blocked0001"}}
	f.folders["subfolder01"] = fakeMFFolder{name: "Part 1/2", files: []string{"subfile0001", "locked00001"}, folders: []string{"deepfolder1"}}
	f.folders["deepfolder1"] = fakeMFFolder{name: "Deep", files: []string{"deepfile001"}}
	f.folders["emptyfolder"] = fakeMFFolder{name: "Empty"}
	f.folders["blocked0001"] = fakeMFFolder{name: "Blocked", files: []string{"rootfile001"}, dmca: true}
	m := newMediafire(t, f)

	items, itemErrs, err := collect(t, m, "https://www.mediafire.com/folder/rootfolder1/Data_Set")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, it := range items {
		got = append(got, it.Dir+"|"+it.Filename)
		if it.Index != i {
			t.Errorf("%s has index %d, want %d", it.Filename, it.Index, i)
		}
	}
	want := []string{
		"Data Set|a.txt", "Data Set|b.txt", "Data Set|c.txt",
		// A "/" inside a folder's own name must not become a level.
		"Data Set/Part 1-2|d.txt",
		"Data Set/Part 1-2/Deep|e.txt",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The locked file and the blocked subfolder are reported, not dropped
	// silently, and they don't stop the rest.
	var reported []string
	for _, e := range itemErrs {
		reported = append(reported, e.URL+": "+e.Err.Error())
	}
	if len(itemErrs) != 2 || !strings.Contains(reported[0], "locked00001") || !strings.Contains(reported[1], "DMCA") {
		t.Errorf("item errors:\n%s", strings.Join(reported, "\n"))
	}

	// An empty subfolder is not listed at all.
	for _, c := range f.calls {
		if strings.Contains(c, "emptyfolder") && strings.Contains(c, "get_content") {
			t.Errorf("an empty folder was listed: %s", c)
		}
	}
}

func TestMediafireEmptyFolderIsAnError(t *testing.T) {
	f := newFakeMediafire()
	f.folders["emptyfolder"] = fakeMFFolder{name: "Empty"}
	m := newMediafire(t, f)
	_, _, err := collect(t, m, "https://www.mediafire.com/folder/emptyfolder")
	if err == nil || !strings.Contains(err.Error(), "no files") {
		t.Errorf("empty folder: %v", err)
	}
}

func TestMediafireRefusesAPageAsTheFile(t *testing.T) {
	m := NewMediafire(mfCfg()).(*mediafire)
	page := &http.Response{Header: http.Header{"Content-Type": {"text/html; charset=UTF-8"}}}
	err := m.ValidateResponse(page)
	var rt interface{ Retryable() bool }
	if !errors.As(err, &rt) || !rt.Retryable() {
		t.Errorf("a web page from the download server: %v, want a retryable error (a fresh link may fix it)", err)
	}
	file := &http.Response{Header: http.Header{"Content-Type": {"application/zip"}}}
	if err := m.ValidateResponse(file); err != nil {
		t.Errorf("a file was refused: %v", err)
	}
	// An uploaded .html comes from the download server as a page: it is the file.
	upload, _ := http.NewRequest(http.MethodGet, "https://download7.mediafire.com/k/abcde12345/index.html", nil)
	html := &http.Response{Header: http.Header{"Content-Type": {"text/html"}}, Request: upload}
	if err := m.ValidateResponse(html); err != nil {
		t.Errorf("an uploaded .html was refused: %v", err)
	}
}

// When the caller asks to stop (yield returns an error, e.g. the quota ran
// out), the walk ends there: the error is not a subfolder that failed, and
// no other folder is listed or yielded.
func TestMediafireStopsWhenAskedInsideASubfolder(t *testing.T) {
	f := newFakeMediafire()
	f.addFile("firstfile01", "x.bin", "x")
	f.addFile("secondfile1", "y.bin", "y")
	f.folders["rootfolder1"] = fakeMFFolder{name: "Root", folders: []string{"subfolder01", "subfolder02"}}
	f.folders["subfolder01"] = fakeMFFolder{name: "One", files: []string{"firstfile01"}}
	f.folders["subfolder02"] = fakeMFFolder{name: "Two", files: []string{"secondfile1"}}
	m := newMediafire(t, f)

	stop := errors.New("stop")
	yields := 0
	itemErrs, err := m.Resolve(context.Background(), "https://www.mediafire.com/folder/rootfolder1", func(Item) error {
		yields++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("Resolve = %v, want the caller's error back", err)
	}
	if yields != 1 || len(itemErrs) != 0 {
		t.Errorf("%d yields, item errors %v; want 1 and none", yields, itemErrs)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "subfolder02") {
			t.Errorf("the walk went on after the stop: %s", c)
		}
	}
}
