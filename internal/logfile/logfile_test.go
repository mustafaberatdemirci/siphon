package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	old := homeDir
	homeDir = func() string { return `C:\Users\someone` }
	t.Cleanup(func() { homeDir = old })

	for _, tc := range []struct{ in, want string }{
		{"https://mega.nz/file/AbCd#k3yK3yK3y", "https://mega.nz/file/AbCd#…"},
		{"https://mega.nz/#!AbCd!k3y failed", "https://mega.nz/#… failed"},
		{"GET https://cdn.example/f.bin?token=s3cret&ex=17&n=2: HTTP 503",
			"GET https://cdn.example/f.bin?token=…&ex=…&n=…: HTTP 503"},
		{"https://user:pa55@host.example/a?x=1#y", "https://…@host.example/a?x=…#…"},
		{"https://example.com/a/b.bin", "https://example.com/a/b.bin"},
		{`saved to C:\Users\someone\Downloads\siphon\a.bin`, `saved to ~\Downloads\siphon\a.bin`},
		{`c:\users\SOMEONE\x`, `~\x`},
		{`"https://h.example/p?q=1"`, `"https://h.example/p?q=…"`},
		{"no link here #1 ?x=2", "no link here #1 ?x=2"},
	} {
		if got := Redact(tc.in); got != tc.want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestPrintfRedactsAndKeepsOneLinePerEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "siphon.log")
	l, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	l.Printf("ERROR %s: %v", "https://mega.nz/file/x#key", "first\nsecond")
	l.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "key") {
		t.Errorf("the key reached the file: %q", s)
	}
	if strings.Count(s, "\n") != 1 || !strings.Contains(s, "first | second") {
		t.Errorf("one entry should be one line: %q", s)
	}
}

func TestRotatesAndTailReadsBothFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "siphon.log")
	l, err := Open(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 20; i++ {
		l.Printf("line %02d", i)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 200 {
		t.Errorf("the log grew to %d bytes past its limit of 200", fi.Size())
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("no older file: %v", err)
	}
	tail := l.Tail(8)
	if len(tail) != 8 || !strings.HasSuffix(tail[7], "line 19") || !strings.HasSuffix(tail[0], "line 12") {
		t.Errorf("Tail(8) = %q", tail)
	}
}

func TestNilFileIsHarmless(t *testing.T) {
	var l *File
	l.Printf("x")
	if l.Tail(3) != nil || l.Close() != nil {
		t.Error("a nil log should do nothing")
	}
}
