package site

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func gdriveCfg() SiteConfig {
	return SiteConfig{
		Name:        GdriveName,
		Domains:     []string{"drive.google.com", "drive.usercontent.google.com", "docs.google.com"},
		CDNPatterns: []string{"drive.usercontent.google.com", "*.googleusercontent.com"},
		CanaryURLs:  []string{"https://drive.google.com/drive/folders/1KpLl_1tcK0eeehzN980zbG-3M2nhbVks"},
	}.WithDefaults()
}

func TestGdriveParse(t *testing.T) {
	g := NewGdrive(gdriveCfg()).(*gdrive)
	const id = "1l_5RK28JRL19wpT22B-DY9We3TVXnnQQ"
	for raw, want := range map[string]gdLink{
		"https://drive.google.com/file/d/" + id + "/view?usp=sharing":        {kind: gdFile, id: id},
		"drive.google.com/file/d/" + id:                                      {kind: gdFile, id: id},
		"https://drive.google.com/open?id=" + id:                             {kind: gdFile, id: id},
		"https://drive.google.com/uc?id=" + id + "&export=download":          {kind: gdFile, id: id},
		"https://drive.usercontent.google.com/download?id=" + id:             {kind: gdFile, id: id},
		"https://drive.google.com/drive/folders/" + id + "?usp=sharing":      {kind: gdFolder, id: id},
		"https://drive.google.com/drive/u/1/folders/" + id:                   {kind: gdFolder, id: id},
		"https://drive.google.com/drive/mobile/folders/" + id:                {kind: gdFolder, id: id},
		"https://drive.google.com/embeddedfolderview?id=" + id:               {kind: gdFolder, id: id},
		"https://docs.google.com/document/d/" + id + "/edit":                 {kind: gdDoc, id: id, doc: "document"},
		"https://docs.google.com/spreadsheets/d/" + id + "/edit#gid=0":       {kind: gdDoc, id: id, doc: "spreadsheets"},
		"https://docs.google.com/presentation/d/" + id + "/edit?usp=sharing": {kind: gdDoc, id: id, doc: "presentation"},
	} {
		got, err := g.parse(raw)
		if err != nil || got != want {
			t.Errorf("parse(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"https://drive.google.com/",
		"https://drive.google.com/file/d/short/view",
		"https://docs.google.com/forms/d/" + id + "/viewform",
		"https://drive.google.com/drive/my-drive",
		"https://example.com/file/d/" + id + "/view",
	} {
		if g.Match(raw) {
			t.Errorf("%q was taken", raw)
		}
	}
}

// folderView is an embedded folder view shaped like the measured one.
func folderView(title string, entries ...[2]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<!DOCTYPE html><html><head><title>%s</title></head><body><div class="flip-entries">`, title)
	for i, e := range entries {
		fmt.Fprintf(&b, `<div class="flip-entry" id="entry-x%d" tabindex="0" role="link"><div class="flip-entry-info"><a href="%s" target="_blank"><div class="flip-entry-visual"></div><div class="flip-entry-title">%s</div></a></div><div class="flip-entry-last-modified"><div>10/29/20</div></div></div>`, i, e[0], e[1])
	}
	b.WriteString(`</div></body></html>`)
	return b.String()
}

const (
	gdRoot  = "1rootFolder000000000000000000000"
	gdSub   = "1subFolder0000000000000000000000"
	gdFileA = "1fileA00000000000000000000000000"
	gdFileB = "1fileB00000000000000000000000000"
	gdDocC  = "1docC000000000000000000000000000"
)

func gdriveRouter() *hostRouter {
	views := map[string]string{
		gdRoot: folderView("Holiday &amp; more",
			[2]string{"https://drive.google.com/drive/folders/" + gdSub, "Day 1"},
			[2]string{"https://drive.google.com/file/d/" + gdFileA + "/view?usp=drive_web", "beach.jpg"},
			[2]string{"https://docs.google.com/document/d/" + gdDocC + "/edit?usp=drive_web", "Plan"},
			[2]string{"https://docs.google.com/forms/d/" + gdDocC + "/edit?usp=drive_web", "Survey"},
		),
		gdSub: folderView("Day 1",
			[2]string{"https://drive.google.com/file/d/" + gdFileB + "/view?usp=drive_web", "sea.jpg"},
			[2]string{"https://drive.google.com/drive/folders/" + gdRoot, "loop back"},
		),
	}
	return &hostRouter{handlers: map[string]http.HandlerFunc{
		"drive.google.com": func(w http.ResponseWriter, r *http.Request) {
			v, ok := views[r.URL.Query().Get("id")]
			if r.URL.Path != "/embeddedfolderview" || !ok {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("<title>Error 404 (Not Found)!!1</title>"))
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(v))
		},
		"drive.usercontent.google.com": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("confirm") != "t" || q.Get("export") != "download" {
				// The virus-scan page a large file gets without confirm=t.
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<title>Google Drive - Virus scan warning</title>"))
				return
			}
			if q.Get("id") != gdFileA {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("<title>Error 404 (Not Found)!!1</title>"))
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="beach.jpg"`)
			w.Header().Set("Content-Range", "bytes 0-0/12345")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("x"))
		},
	}}
}

// A folder: its files and documents, then its subfolders' (each folder once,
// even when a shortcut leads back), in folders named after them; entries of
// other kinds are said to be left out.
func TestGdriveFolder(t *testing.T) {
	cfg := gdriveCfg()
	cfg.HTTPClient = &http.Client{Transport: gdriveRouter()}
	g := NewGdrive(cfg)
	var items []Item
	itemErrs, err := g.Resolve(context.Background(), "https://drive.google.com/drive/folders/"+gdRoot, func(it Item) error {
		items = append(items, it)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ dir, name, url string }{
		{"Holiday & more", "beach.jpg", gdFileURL(gdFileA)},
		{"Holiday & more", "Plan.docx", "https://docs.google.com/document/d/" + gdDocC + "/export?format=docx"},
		{"Holiday & more/Day 1", "sea.jpg", gdFileURL(gdFileB)},
	}
	if len(items) != len(want) {
		t.Fatalf("items: %+v", items)
	}
	for i, w := range want {
		it := items[i]
		if it.Dir != w.dir || it.Filename != w.name || it.URL != w.url || it.Size != -1 || it.Index != i {
			t.Errorf("[%d] = %+v, want %+v", i, it, w)
		}
	}
	if items[0].SourcePage != gdFileView(gdFileA) {
		t.Errorf("source page %q", items[0].SourcePage)
	}
	if len(itemErrs) != 1 || !strings.Contains(itemErrs[0].Err.Error(), "can't download") {
		t.Errorf("the form wasn't reported: %v", itemErrs)
	}
}

// A file: its first byte names it and gives its size, over confirm=t; a
// missing one is said to be a page, not a file.
func TestGdriveFile(t *testing.T) {
	cfg := gdriveCfg()
	cfg.HTTPClient = &http.Client{Transport: gdriveRouter()}
	g := NewGdrive(cfg)
	items, _, err := collect(t, g, "https://drive.google.com/file/d/"+gdFileA+"/view?usp=sharing")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Filename != "beach.jpg" || items[0].Size != 12345 ||
		items[0].URL != gdFileURL(gdFileA) || items[0].SourcePage != gdFileView(gdFileA) {
		t.Errorf("file: %+v", items)
	}
	_, _, err = collect(t, g, "https://drive.google.com/file/d/"+gdFileB+"0/view")
	if err == nil || !strings.Contains(err.Error(), "answered with a page") {
		t.Errorf("missing file: %v", err)
	}
	if _, _, err := collect(t, g, "https://drive.google.com/drive/folders/"+gdFileB+"00"); err == nil || !strings.Contains(err.Error(), "doesn't exist") {
		t.Errorf("missing folder: %v", err)
	}
}

func TestGdriveValidateResponse(t *testing.T) {
	g := NewGdrive(gdriveCfg()).(*gdrive)
	mk := func(ctype, cd string) *http.Response {
		h := http.Header{}
		h.Set("Content-Type", ctype)
		if cd != "" {
			h.Set("Content-Disposition", cd)
		}
		return &http.Response{Header: h}
	}
	if g.ValidateResponse(mk("text/html; charset=utf-8", "")) == nil {
		t.Error("a quota page was taken as the file")
	}
	if err := g.ValidateResponse(mk("text/html", `attachment; filename="index.html"`)); err != nil {
		t.Errorf("an uploaded page was refused: %v", err)
	}
	if err := g.ValidateResponse(mk("application/octet-stream", "")); err != nil {
		t.Errorf("a file was refused: %v", err)
	}
}
