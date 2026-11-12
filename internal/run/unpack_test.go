package run

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// With -extract, a downloaded zip is unpacked once the link is done; with
// -extract-delete the archive goes after.
func TestDownloadedArchiveIsUnpacked(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"a.txt", "b.txt"} {
		w, _ := zw.Create(n)
		_, _ = w.Write([]byte("content of " + n))
	}
	_ = zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		http.ServeContent(w, r, "", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), bytes.NewReader(buf.Bytes()))
	}))
	t.Cleanup(srv.Close)

	for _, del := range []bool{false, true} {
		out := tempDir(t)
		rc := directRunCtx(t, out, srv.URL+"/files/Loose.zip", srv.Client())
		rc.opt.Unpack, rc.opt.UnpackDelete = true, del
		var sum Summary
		if res := runURL(context.Background(), rc, &sum); res.done != 1 {
			t.Fatalf("run: %+v", res)
		}
		got, err := os.ReadFile(filepath.Join(out, "Loose", "b.txt"))
		if err != nil || string(got) != "content of b.txt" {
			t.Errorf("delete=%v: unpacked %q, %v", del, got, err)
		}
		_, err = os.Stat(filepath.Join(out, "Loose.zip"))
		if del != os.IsNotExist(err) {
			t.Errorf("delete=%v: the archive is there: %v", del, err == nil)
		}
	}
}
