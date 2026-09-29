package site

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func cyberdropCfg() SiteConfig {
	return SiteConfig{
		Name:          CyberdropName,
		Domains:       []string{"cyberdrop.cr"},
		LegacyDomains: []string{"cyberdrop.me", "cyberdrop.to"},
		CDNPatterns:   []string{"*.cdn.gigachad-cdn.ru"},
	}.WithDefaults()
}

// cdFileHTML is one file of the no-JavaScript album page, in the layout
// measured on 2026-09-29 (trimmed). size < 0 leaves the size line out.
func cdFileHTML(id, name string, size int64) string {
	sizeLine := ""
	if size >= 0 {
		sizeLine = fmt.Sprintf(`<p class="is-hidden file-size">%d B</p>`, size)
	}
	return fmt.Sprintf(`
      <div class="image-container column">
        <a class="image" href="/f/%[1]s" taget="_blank" title="" rel="noopener noreferrer" data-type="img">
          <img alt="%[2]s" src="https://cyberdrop.cr/thumbs/x.png" loading="lazy" />
        </a>
        <div class="details">
          <p><span class="name"><a id="file" href="/f/%[1]s" target="_blank" title="%[2]s" rel="noopener noreferrer">%[2]s</a></span></p>
          %[3]s
	  <p>1 KB</p>
        </div>
      </div>`, id, name, sizeLine)
}

func cdAlbumHTML(title string, count int, files ...string) string {
	return fmt.Sprintf(`<html><body><div class="container">
    <h1 id="title" class="title has-text-centered" title="%s">
        %s
      </h1>
    <h1 id="count" class="subtitle is-hidden-desktop has-text-centered">
       %d files (1 KB)<br>
    </h1>
    <div id="table" class="columns is-multiline is-mobile is-centered has-text-centered">%s
    </div></div></body></html>`, title, title, count, strings.Join(files, ""))
}

type fakeCyberdrop struct {
	albums map[string]string // id -> page
	files  map[string]string // id -> name
	// authFails makes file/auth answer with this status.
	authFails int
	calls     []string
}

func (f *fakeCyberdrop) router() *hostRouter {
	return &hostRouter{handlers: map[string]http.HandlerFunc{
		"cyberdrop.cr": func(w http.ResponseWriter, r *http.Request) {
			f.calls = append(f.calls, "page "+r.URL.RequestURI())
			id, ok := strings.CutPrefix(r.URL.Path, "/a/")
			page, found := f.albums[id]
			if !ok || !found || r.URL.RawQuery != "nojs" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(page))
		},
		"api.cyberdrop.cr": func(w http.ResponseWriter, r *http.Request) {
			f.calls = append(f.calls, "api "+r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasPrefix(r.URL.Path, "/api/file/info/"):
				id := strings.TrimPrefix(r.URL.Path, "/api/file/info/")
				name, ok := f.files[id]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"error":"File not found"}`)
					return
				}
				fmt.Fprintf(w, `{"name":%q,"type":"image/png","size":4242,"slug":%q}`, name, id)
			case strings.HasPrefix(r.URL.Path, "/api/file/auth/"):
				id := strings.TrimPrefix(r.URL.Path, "/api/file/auth/")
				if f.authFails != 0 {
					w.WriteHeader(f.authFails)
					fmt.Fprint(w, `{"error":"Failed to generate signed URL"}`)
					return
				}
				fmt.Fprintf(w, `{"url":"https://k1-cd.cdn.gigachad-cdn.ru/api/file/d/%s?token=signed"}`, id)
			default:
				http.NotFound(w, r)
			}
		},
	}}
}

func newCyberdrop(t *testing.T, f *fakeCyberdrop) *cyberdrop {
	t.Helper()
	cfg := cyberdropCfg()
	cfg.HTTPClient = &http.Client{Transport: f.router()}
	return NewCyberdrop(cfg).(*cyberdrop)
}

func TestCyberdropMatch(t *testing.T) {
	c := NewCyberdrop(cyberdropCfg())
	for _, u := range []string{
		"https://cyberdrop.cr/a/8uE0wQiK",
		"cyberdrop.cr/a/8uE0wQiK",
		"https://cyberdrop.me/a/8uE0wQiK",
		"https://cyberdrop.to/a/8uE0wQiK/",
		"https://cyberdrop.cr/f/urLPkBXGuNfEg",
		"https://cyberdrop.me/e/lHYBt9VAluZf6",
		"https://api.cyberdrop.cr/api/file/info/urLPkBXGuNfEg",
		"https://api.cyberdrop.cr/api/file/auth/urLPkBXGuNfEg",
		"https://k1-cd.cdn.gigachad-cdn.ru/api/file/d/urLPkBXGuNfEg?token=abc",
	} {
		if !c.Match(u) {
			t.Errorf("Match(%q) = false, want true", u)
		}
	}
	for _, u := range []string{
		"https://cyberdrop.cr/",
		"https://cyberdrop.cr/a/",
		"https://cyberdrop.cr/f/urLPkBXGuNfEg/extra",
		"https://cyberdrop.cr/upload",
		"https://api.cyberdrop.cr/api/album/list",
		// SECURITY: only cyberdrop's own hosts.
		"https://cyberdrop.cr.evil.test/a/8uE0wQiK",
		"https://evil.test/api/file/d/urLPkBXGuNfEg",
		"",
	} {
		if c.Match(u) {
			t.Errorf("Match(%q) = true, want false", u)
		}
	}
}

func TestCyberdropAlbumIsOneRequest(t *testing.T) {
	f := &fakeCyberdrop{albums: map[string]string{
		"Album123": cdAlbumHTML("Holiday &quot;2026&quot; &amp; more / misc", 3,
			cdFileHTML("fileAAAA1", "beach.jpg", 1111),
			// Without a size of its own a file must not take the next one's.
			cdFileHTML("fileCCCC3", "no size.gif", -1),
			cdFileHTML("fileBBBB2", "a&amp;b.png", 2222)),
	}}
	c := newCyberdrop(t, f)

	// An old domain is recognized; the request goes to the current one.
	items, itemErrs, err := collect(t, c, "https://cyberdrop.me/a/Album123")
	if err != nil || len(itemErrs) != 0 {
		t.Fatalf("%v %v", err, itemErrs)
	}
	var got []string
	for i, it := range items {
		got = append(got, fmt.Sprintf("%s|%s|%d|%s", it.Dir, it.Filename, it.Size, it.URL))
		if it.Index != i || it.SourcePage != it.URL {
			t.Errorf("%s: index %d, source page %s", it.Filename, it.Index, it.SourcePage)
		}
	}
	want := []string{
		`Holiday "2026" & more - misc|beach.jpg|1111|https://cyberdrop.cr/f/fileAAAA1`,
		`Holiday "2026" & more - misc|no size.gif|-1|https://cyberdrop.cr/f/fileCCCC3`,
		`Holiday "2026" & more - misc|a&b.png|2222|https://cyberdrop.cr/f/fileBBBB2`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(f.calls) != 1 || f.calls[0] != "page /a/Album123?nojs" {
		t.Errorf("requests %v, want the album page only", f.calls)
	}
}

func TestCyberdropAlbumProblems(t *testing.T) {
	f := &fakeCyberdrop{albums: map[string]string{
		"Short123": cdAlbumHTML("Short", 3, cdFileHTML("fileAAAA1", "a.jpg", 1), cdFileHTML("fileBBBB2", "b.jpg", 2)),
		"Empty123": cdAlbumHTML("Empty", 0),
		"Other123": "<html>a page that is not an album</html>",
	}}
	c := newCyberdrop(t, f)

	items, itemErrs, err := collect(t, c, "https://cyberdrop.cr/a/Short123")
	if err != nil || len(items) != 2 || len(itemErrs) != 1 || !strings.Contains(itemErrs[0].Err.Error(), "count=3, listed=2") {
		t.Errorf("fewer files than claimed: %d items, %v, %v", len(items), itemErrs, err)
	}
	if _, _, err := collect(t, c, "https://cyberdrop.cr/a/Empty123"); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Errorf("empty album: %v", err)
	}
	if _, _, err := collect(t, c, "https://cyberdrop.cr/a/Other123"); err == nil || !strings.Contains(err.Error(), "layout") {
		t.Errorf("not an album page: %v", err)
	}
	if _, _, err := collect(t, c, "https://cyberdrop.cr/a/Gone1234"); err == nil || !strings.Contains(err.Error(), "doesn't exist") {
		t.Errorf("missing album: %v", err)
	}
}

func TestCyberdropFileAndSignedLink(t *testing.T) {
	f := &fakeCyberdrop{files: map[string]string{"fileAAAA1": "photo.png"}}
	c := newCyberdrop(t, f)
	ctx := context.Background()

	items, _, err := collect(t, c, "https://cyberdrop.cr/e/fileAAAA1")
	if err != nil || len(items) != 1 {
		t.Fatalf("%d items, %v", len(items), err)
	}
	it := items[0]
	if it.Filename != "photo.png" || it.Size != 4242 || it.Dir != "" || it.URL != "https://cyberdrop.cr/f/fileAAAA1" {
		t.Errorf("item = %+v", it)
	}

	// The signed link is asked for just before the download.
	link, err := c.PrepareURL(ctx, it.URL)
	if err != nil || link != "https://k1-cd.cdn.gigachad-cdn.ru/api/file/d/fileAAAA1?token=signed" {
		t.Errorf("PrepareURL = %q, %v", link, err)
	}
	// A download server link is already one: no API call to replace it.
	earlier := "https://k1-cd.cdn.gigachad-cdn.ru/api/file/d/fileAAAA1?token=earlier"
	if again, _ := c.PrepareURL(ctx, earlier); again != earlier {
		t.Errorf("a download server link was changed: %q", again)
	}

	var cdErr *CyberdropError
	if _, _, err := collect(t, c, "https://cyberdrop.cr/f/Missing1"); !errors.As(err, &cdErr) || cdErr.Status != 404 || cdErr.Retryable() {
		t.Errorf("missing file: %v", err)
	}
	f.authFails = http.StatusInternalServerError
	_, err = c.PrepareURL(ctx, it.URL)
	var rt interface{ Retryable() bool }
	if !errors.As(err, &rt) || !rt.Retryable() {
		t.Errorf("the signing failing on the server: %v, want a retryable error", err)
	}
}

func TestCyberdropRefusesTheSiteInsteadOfTheFile(t *testing.T) {
	c := NewCyberdrop(cyberdropCfg()).(*cyberdrop)
	site, _ := http.NewRequest(http.MethodGet, "https://cyberdrop.cr/", nil)
	if err := c.ValidateResponse(&http.Response{Header: http.Header{"Content-Type": {"text/html"}}, Request: site}); err == nil {
		t.Error("the site's page was taken for the file")
	}
	cdn, _ := http.NewRequest(http.MethodGet, "https://k1-cd.cdn.gigachad-cdn.ru/api/file/d/x", nil)
	if err := c.ValidateResponse(&http.Response{Header: http.Header{"Content-Type": {"text/html"}}, Request: cdn}); err != nil {
		t.Errorf("an uploaded .html from the download server was refused: %v", err)
	}
}
