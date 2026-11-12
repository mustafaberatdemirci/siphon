package extract

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

func TestOf(t *testing.T) {
	for name, want := range map[string]struct {
		kind Kind
		part Part
	}{
		"photos.zip":             {Zip, Part{}},
		"backup.tar":             {Tar, Part{}},
		"backup.tar.gz":          {TarGz, Part{}},
		"backup.TGZ":             {TarGz, Part{}},
		"backup.tar.bz2":         {TarBz2, Part{}},
		"backup.tar.xz":          {SevenZip, Part{}},
		"movie.7z":               {SevenZip, Part{}},
		"movie.rar":              {SevenZip, Part{Set: "movie.rar", Number: 1}},
		"Movie.part1.rar":        {SevenZip, Part{Set: "movie.rar", Number: 1}},
		"Movie.part02.rar":       {SevenZip, Part{Set: "movie.rar", Number: 2}},
		"movie.r00":              {SevenZip, Part{Set: "movie.rar", Number: 2}},
		"movie.7z.001":           {SevenZip, Part{Set: "movie.7z", Number: 1}},
		"movie.zip.003":          {SevenZip, Part{Set: "movie.zip", Number: 3}},
		"notes.txt":              {None, Part{}},
		"setup.exe":              {None, Part{}},
		"album.part1.rar.backup": {None, Part{}},
	} {
		kind, part := Of(name)
		if kind != want.kind || part != want.part {
			t.Errorf("Of(%q) = %v, %+v; want %v, %+v", name, kind, part, want.kind, want.part)
		}
	}
}

func TestBaseName(t *testing.T) {
	for in, want := range map[string]string{
		"Holiday.zip": "Holiday", "backup.tar.gz": "backup", "Movie.part1.rar": "Movie",
		"movie.7z.001": "movie", "a.b.7z": "a.b", "noext": "noext",
	} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}

// makeZip writes a zip of name -> content ("" content and a trailing slash
// for a folder).
func makeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(files[n]))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// One folder inside goes next to the archive; several things go into a
// folder named after it; nothing in place is overwritten.
func TestExtractPlacesTheContent(t *testing.T) {
	dir := testutil.TempDir(t)
	one := filepath.Join(dir, "Album.zip")
	makeZip(t, one, map[string]string{"Album/a.jpg": "A", "Album/sub/b.jpg": "B"})
	got, err := Extract(context.Background(), one, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "Album") || read(t, filepath.Join(got, "sub", "b.jpg")) != "B" {
		t.Errorf("one folder inside went to %s", got)
	}

	many := filepath.Join(dir, "Loose files.zip")
	makeZip(t, many, map[string]string{"a.txt": "a", "b.txt": "b"})
	got, err = Extract(context.Background(), many, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "Loose files") || read(t, filepath.Join(got, "a.txt")) != "a" {
		t.Errorf("several things went to %s", got)
	}

	// Again: the names are taken now.
	got, err = Extract(context.Background(), one, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "Album (2)") {
		t.Errorf("a second unpacking went to %s, want Album (2)", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".siphon-unpack-*")); len(left) != 0 {
		t.Errorf("work folders left behind: %v", left)
	}
}

// What would land outside the folder is left out ("zip slip").
func TestEntriesStayInside(t *testing.T) {
	dir := testutil.TempDir(t)
	inner := filepath.Join(dir, "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(inner, "evil.zip")
	makeZip(t, evil, map[string]string{
		"../escaped.txt": "x", "/abs.txt": "x", `..\back.txt`: "x", "C:/drive.txt": "x",
		"ok/fine.txt": "fine", "con.txt": "reserved",
	})
	got, err := Extract(context.Background(), evil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "escaped.txt"), filepath.Join(dir, "back.txt")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was written outside the folder", p)
		}
	}
	if read(t, filepath.Join(got, "ok", "fine.txt")) != "fine" {
		t.Error("a safe entry is missing")
	}
	if _, err := os.Stat(filepath.Join(got, "_con.txt")); err != nil {
		t.Errorf("a reserved Windows name wasn't made safe: %v", err)
	}
}

func TestEncryptedZipIsSaidSo(t *testing.T) {
	dir := testutil.TempDir(t)
	p := filepath.Join(dir, "secret.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "a.txt", Method: zip.Store, Flags: 0x1})
	_, _ = w.Write([]byte("encrypted bytes"))
	_ = zw.Close()
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(context.Background(), p, Options{}); !errors.Is(err, ErrEncrypted) {
		t.Errorf("err = %v, want ErrEncrypted", err)
	}
}

func TestArchiveBombStops(t *testing.T) {
	oldR, oldS := maxRatio, maxSlack
	maxRatio, maxSlack = 2, 0
	defer func() { maxRatio, maxSlack = oldR, oldS }()
	dir := testutil.TempDir(t)
	p := filepath.Join(dir, "bomb.zip")
	makeZip(t, p, map[string]string{"zeros.bin": strings.Repeat("\x00", 1<<20)})
	if _, err := Extract(context.Background(), p, Options{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 1 {
		t.Errorf("a stopped unpacking left %v", left)
	}
}

func TestTarGzSkipsLinks(t *testing.T) {
	dir := testutil.TempDir(t)
	p := filepath.Join(dir, "pack.tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	add(&tar.Header{Name: "pack/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "pack/readme.txt", Typeflag: tar.TypeReg, Mode: 0o644}, "hello")
	add(&tar.Header{Name: "pack/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, "")
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Extract(context.Background(), p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(got, "readme.txt")) != "hello" {
		t.Error("the file is missing")
	}
	if _, err := os.Lstat(filepath.Join(got, "link")); err == nil {
		t.Error("a link was unpacked")
	}
}

func TestNeeds7Zip(t *testing.T) {
	dir := testutil.TempDir(t)
	p := filepath.Join(dir, "movie.7z")
	if err := os.WriteFile(p, []byte("7z"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(context.Background(), p, Options{}); !errors.Is(err, ErrNeeds7Zip) {
		t.Errorf("err = %v, want ErrNeeds7Zip", err)
	}
}

// find7z is 7-Zip on this machine, or "".
func find7z() string {
	for _, n := range []string{"7z", "7za"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{`C:\Program Files\7-Zip\7z.exe`, `C:\Program Files (x86)\7-Zip\7z.exe`} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// With 7-Zip on the machine: a 7z in parts unpacks from its first part,
// and a password-protected one is said so.
func TestSevenZipParts(t *testing.T) {
	sz := find7z()
	if sz == "" {
		t.Skip("7-Zip isn't installed")
	}
	dir := testutil.TempDir(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "Show"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("siphon 7z part test "), 20000) // ~400 KB, split in 3
	if err := os.WriteFile(filepath.Join(src, "Show", "ep1.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "dl")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command(sz, args...)
		cmd.Dir = src
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("7z %v: %v\n%s", args, err, b)
		}
	}
	run("a", "-mx0", "-v150k", filepath.Join(out, "Show.7z"), "Show")
	if _, err := os.Stat(filepath.Join(out, "Show.7z.003")); err != nil {
		t.Fatalf("the test archive wasn't split in parts: %v", err)
	}
	got, err := Extract(context.Background(), filepath.Join(out, "Show.7z.001"), Options{SevenZip: sz})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(got, "ep1.bin")); err != nil || !bytes.Equal(b, big) {
		t.Errorf("the unpacked file is wrong (%v) in %s", err, got)
	}

	run("a", "-psecret", "-mhe=on", filepath.Join(out, "Locked.7z"), "Show")
	if _, err := Extract(context.Background(), filepath.Join(out, "Locked.7z"), Options{SevenZip: sz}); !errors.Is(err, ErrEncrypted) {
		t.Errorf("a password-protected 7z gave %v, want ErrEncrypted", err)
	}
}

func TestArchives(t *testing.T) {
	d := filepath.Join("out", "Album")
	in := []string{
		filepath.Join(d, "Show.part2.rar"), filepath.Join(d, "notes.txt"), filepath.Join(d, "Show.part1.rar"),
		filepath.Join(d, "photos.zip"), filepath.Join(d, "Lone.7z.002"), filepath.Join("out", "Other", "Show.part1.rar"),
	}
	got := Archives(in)
	want := []Archive{
		{First: in[2], Parts: []string{in[0], in[2]}},
		{First: in[3], Parts: []string{in[3]}},
		{First: in[5], Parts: []string{in[5]}},
	}
	if len(got) != len(want) {
		t.Fatalf("Archives = %+v", got)
	}
	for i := range want {
		if got[i].First != want[i].First || strings.Join(got[i].Parts, "|") != strings.Join(want[i].Parts, "|") {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
