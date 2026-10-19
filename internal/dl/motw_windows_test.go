package dl

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/site"
)

// A finished download carries the mark browsers put on downloaded files, so
// Windows warns before starting it; a file that was already there and only
// counted as done is left as it was.
func TestDownloadIsMarkedAsFromTheInternet(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	out := tempDir(t)
	d := &Downloader{Client: srv.Client()}
	it := site.Item{URL: srv.URL + "/f", SourcePage: "https://mega.nz/file/AbCd#secret", Filename: "a.bin", Size: int64(len(body))}

	res, err := d.Download(context.Background(), out, it)
	if err != nil {
		t.Fatal(err)
	}
	mark, err := os.ReadFile(res.Path + ":Zone.Identifier")
	if err != nil {
		t.Fatalf("no Zone.Identifier: %v", err)
	}
	s := string(mark)
	if !strings.Contains(s, "ZoneId=3") || !strings.Contains(s, "HostUrl=https://mega.nz/file/AbCd") {
		t.Errorf("mark = %q", s)
	}
	if strings.Contains(s, "secret") {
		t.Errorf("the mark carries the link's key: %q", s)
	}

	own := filepath.Join(out, "own.bin")
	if err := os.WriteFile(own, body, 0o644); err != nil {
		t.Fatal(err)
	}
	it.Filename, it.SourcePage = "own.bin", "https://example.test/own"
	if _, err := d.Download(context.Background(), out, it); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(own + ":Zone.Identifier"); err == nil {
		t.Error("a file that was already there got marked")
	}
}
