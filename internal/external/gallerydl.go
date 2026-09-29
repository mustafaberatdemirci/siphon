package external

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GalleryDLProbe reports whether gallery-dl recognizes the link, by
// simulating the first file only. nil means yes; ErrUnsupported means no.
//
// MEASURED (gallery-dl 1.32.14): a supported link exits 0 and prints
// "# <name>" for the simulated file; an unknown one exits 64 with
// "[gallery-dl][error] Unsupported URL '…'".
func (t Tools) GalleryDLProbe(ctx context.Context, link string) error {
	if t.GalleryDL == "" {
		return ErrUnsupported
	}
	_, stderr, err := output(command(ctx, t.GalleryDL, "--simulate", "--range", "1", "--", link))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.Contains(stderr, "Unsupported URL") {
			return ErrUnsupported
		}
		return fmt.Errorf("gallery-dl: %s", lastLineWith(stderr, "error"))
	}
	return nil
}

// GalleryDLDownload downloads everything behind the link under base (in
// gallery-dl's own folders, e.g. base/imgur/…) and returns the folder the
// files landed in and their total size. progress gets the bytes on disk so
// far; the total is unknown (-1), gallery-dl doesn't count ahead.
//
// Files already there are skipped by gallery-dl itself, so a paused or
// failed gallery continues where it stopped when it runs again.
//
// MEASURED: every downloaded file is printed on stdout as its full path, and
// every skipped one as "# <path>".
func (t Tools) GalleryDLDownload(ctx context.Context, link, base string, progress func(done, total int64)) (dir string, size int64, err error) {
	if t.GalleryDL == "" {
		return "", 0, ErrUnsupported
	}
	var first string
	var count int
	stderr, err := run(command(ctx, t.GalleryDL, "-d", base, "--", link), func(line string) {
		p := strings.TrimSpace(strings.TrimPrefix(line, "# "))
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		fi, serr := os.Stat(p)
		if serr != nil || fi.IsDir() {
			return
		}
		if first == "" {
			first = p
		}
		count++
		size += fi.Size()
		if progress != nil {
			progress(size, -1)
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		msg := lastLineWith(stderr, "error")
		if count > 0 {
			return "", 0, fmt.Errorf("gallery-dl: %d files downloaded, then: %s (run it again to continue)", count, msg)
		}
		return "", 0, fmt.Errorf("gallery-dl: %s", msg)
	}
	if count == 0 {
		return "", 0, fmt.Errorf("gallery-dl found nothing to download")
	}
	return filepath.Dir(first), size, nil
}
