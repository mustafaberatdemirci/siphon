// Package extract unpacks a finished download that is an archive.
//
// zip and tar archives (plain, gzip or bzip2) are read with Go's own
// packages, so they need nothing installed. 7z, rar, tar.xz and archives in
// parts (name.part1.rar, name.7z.001, name.zip.001) go to 7-Zip when it is
// installed.
//
// An archive is unpacked into a hidden folder next to it first and moved
// into place once whole, so a stopped or failed unpacking leaves nothing
// half there. What is inside is kept inside: an entry naming a parent
// folder or an absolute path is refused, links are skipped, names are made
// safe for Windows as downloads' are, and unpacking stops when it grows
// past a multiple of the archive's size (an archive bomb).
package extract

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mustafaberatdemirci/siphon/internal/dl"
)

// Kind is how an archive is read.
type Kind int

const (
	None     Kind = iota // not an archive
	Zip                  // archive/zip
	Tar                  // archive/tar
	TarGz                // tar in gzip
	TarBz2               // tar in bzip2
	SevenZip             // 7z, rar, tar.xz and archives in parts: 7-Zip
)

// Part is where a name stands in an archive in parts: Set names the set
// (the same for every part, case folded), Number counts from 1. The zero
// Part is a whole archive.
type Part struct {
	Set    string
	Number int
}

var (
	partRar  = regexp.MustCompile(`(?i)^(.+)\.part0*(\d+)\.rar$`)
	numbered = regexp.MustCompile(`(?i)^(.+\.(?:7z|zip|rar))\.(\d{3})$`)
	oldRar   = regexp.MustCompile(`(?i)^(.+)\.r(\d{2})$`)
)

// Of tells how a file named name is read, and its part if it is one.
func Of(name string) (Kind, Part) {
	low := strings.ToLower(name)
	if m := partRar.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[2])
		return SevenZip, Part{Set: strings.ToLower(m[1]) + ".rar", Number: n}
	}
	if m := numbered.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[2])
		return SevenZip, Part{Set: strings.ToLower(m[1]), Number: n}
	}
	if m := oldRar.FindStringSubmatch(name); m != nil {
		// name.rar is part 1, name.r00 part 2, name.r01 part 3...
		n, _ := strconv.Atoi(m[2])
		return SevenZip, Part{Set: strings.ToLower(m[1]) + ".rar", Number: n + 2}
	}
	switch {
	case strings.HasSuffix(low, ".tar.gz"), strings.HasSuffix(low, ".tgz"):
		return TarGz, Part{}
	case strings.HasSuffix(low, ".tar.bz2"), strings.HasSuffix(low, ".tbz2"), strings.HasSuffix(low, ".tbz"):
		return TarBz2, Part{}
	case strings.HasSuffix(low, ".tar.xz"), strings.HasSuffix(low, ".txz"):
		return SevenZip, Part{}
	case strings.HasSuffix(low, ".tar"):
		return Tar, Part{}
	case strings.HasSuffix(low, ".zip"):
		return Zip, Part{}
	case strings.HasSuffix(low, ".7z"):
		return SevenZip, Part{}
	case strings.HasSuffix(low, ".rar"):
		// The first part of an old-style set (name.rar, name.r00...) is a
		// whole-looking name; the caller finds the rest by the set.
		return SevenZip, Part{Set: low, Number: 1}
	}
	return None, Part{}
}

// baseName is the archive's name without its archive endings: the folder
// it is unpacked into when it holds more than one thing.
func baseName(name string) string {
	if m := partRar.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	if m := numbered.FindStringSubmatch(name); m != nil {
		name = m[1]
	}
	low := strings.ToLower(name)
	for _, ext := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tgz", ".tbz2", ".tbz", ".txz", ".tar", ".zip", ".7z", ".rar"} {
		if strings.HasSuffix(low, ext) && len(name) > len(ext) {
			return name[:len(name)-len(ext)]
		}
	}
	return name
}

// Errors the caller tells the user apart.
var (
	ErrEncrypted = errors.New("the archive is password-protected")
	ErrNeeds7Zip = errors.New("7-Zip is needed for this archive and wasn't found")
	ErrTooLarge  = errors.New("the archive unpacks to far more than its size (an archive bomb?)")
)

// maxRatio bounds what an archive may unpack to: this many times its size,
// plus maxSlack. Real archives stay far below; a bomb doesn't. Variables
// so a test can lower them.
var (
	maxRatio int64 = 200
	maxSlack int64 = 64 << 20
)

// Options say how to unpack.
type Options struct {
	// SevenZip is 7-Zip's program ("" if it isn't installed).
	SevenZip string
	// Source is the page the archive came from: unpacked files are marked
	// as downloaded from it (Windows' Mark of the Web), as the archive was.
	Source string
}

// Extract unpacks the archive at first (a whole archive, or the first part
// of a set) next to it and returns where its content went: the one folder
// or file it holds, or a folder named after it. Nothing already there is
// overwritten: a name in use gets " (2)", " (3)"...
func Extract(ctx context.Context, first string, o Options) (string, error) {
	kind, _ := Of(filepath.Base(first))
	if kind == None {
		return "", fmt.Errorf("%s is not an archive Siphon unpacks", filepath.Base(first))
	}
	fi, err := os.Stat(first)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(first)
	tmp, err := os.MkdirTemp(dir, ".siphon-unpack-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // empty by then when it worked

	limit := fi.Size()*maxRatio + maxSlack
	switch kind {
	case Zip:
		err = unzip(ctx, first, tmp, limit)
	case Tar, TarGz, TarBz2:
		err = untar(ctx, first, tmp, kind, limit)
	case SevenZip:
		err = un7z(ctx, o.SevenZip, first, tmp)
	}
	if err != nil {
		return "", err
	}
	if err := dropUnsafe(tmp); err != nil {
		return "", err
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		return "", err
	}
	var from, name string
	switch len(entries) {
	case 0:
		return "", errors.New("the archive is empty")
	case 1:
		from, name = filepath.Join(tmp, entries[0].Name()), entries[0].Name()
		// A lone file lands in the download folder itself: not as one of
		// the programs or the settings file Siphon would take from there.
		if !entries[0].IsDir() {
			if err := dl.CheckProtected(filepath.Join(dir, name)); err != nil {
				return "", err
			}
		}
	default:
		from, name = tmp, dl.Component(baseName(filepath.Base(first)))
		if name == "" {
			name = "unpacked"
		}
	}
	dest := unused(filepath.Join(dir, name))
	if err := os.Rename(from, dest); err != nil {
		return "", err
	}
	markTree(dest, o.Source)
	return dest, nil
}

// unused is p, or p with " (2)", " (3)"... before its extension if p is taken.
func unused(p string) string {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return p
	}
	ext := filepath.Ext(p)
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		ext = ""
	}
	stem := strings.TrimSuffix(p, ext)
	for n := 2; ; n++ {
		c := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		if _, err := os.Lstat(c); errors.Is(err, fs.ErrNotExist) {
			return c
		}
	}
}

// safePath turns an archive entry's name into a path under root, or ""
// when the name can't be one: absolute, naming a parent, or empty once
// made safe. Every level is made safe for Windows as a download's is.
func safePath(root, name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	if path.IsAbs(name) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return ""
	}
	var parts []string
	for _, level := range strings.Split(name, "/") {
		switch level {
		case "", ".":
			continue
		case "..":
			return ""
		}
		c := dl.Component(level)
		if c == "" {
			return ""
		}
		parts = append(parts, c)
	}
	if len(parts) == 0 {
		return ""
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// budget counts the bytes written against the archive's limit.
type budget struct{ left int64 }

func (b *budget) copy(ctx context.Context, w io.Writer, r io.Reader) error {
	buf := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if b.left -= int64(n); b.left < 0 {
				return ErrTooLarge
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func writeFile(ctx context.Context, b *budget, dst string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := b.copy(ctx, f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func unzip(ctx context.Context, archive, root string, limit int64) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("not a readable zip: %w", err)
	}
	defer zr.Close()
	b := &budget{left: limit}
	for _, f := range zr.File {
		if f.Flags&0x1 != 0 {
			return ErrEncrypted
		}
		dst := safePath(root, f.Name)
		mode := f.Mode()
		switch {
		case dst == "", mode&fs.ModeSymlink != 0, mode&fs.ModeType != 0 && !mode.IsDir():
			continue // outside the folder, a link, or not a file: left out
		case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(ctx, b, dst, rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func untar(ctx context.Context, archive, root string, kind Kind, limit int64) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	switch kind {
	case TarGz:
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("not a readable gzip: %w", err)
		}
		defer gz.Close()
		r = gz
	case TarBz2:
		r = bzip2.NewReader(f)
	}
	tr := tar.NewReader(r)
	b := &budget{left: limit}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("not a readable tar: %w", err)
		}
		dst := safePath(root, h.Name)
		if dst == "" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(ctx, b, dst, tr); err != nil {
				return err
			}
		default:
			// links, devices, fifos: left out
		}
	}
}

// dropUnsafe removes what doesn't belong in a download folder from an
// unpacked tree: links (7-Zip may make them), which could point anywhere.
func dropUnsafe(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return os.Remove(p)
		}
		return nil
	})
}

// markTree marks every file under p (or p itself) as downloaded from source.
func markTree(p, source string) {
	_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			dl.MarkDownloaded(q, source)
		}
		return nil
	})
}
