package browser

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractLinks(t *testing.T) {
	text := `Here: https://mega.nz/file/AbCd#k3y-Xy_z, and (https://gofile.io/d/abcd).
mega.nz/folder/XyZw#k3y
"https://cyberfile.me/kwNx" <https://bunkr.ws/a/Q1w2>
yt-dlp:https://video.example/watch?v=1!
https://mega.nz/file/AbCd#k3y-Xy_z again
ftp://not.this/x javascript:alert(1)`
	want := []string{
		"https://mega.nz/file/AbCd#k3y-Xy_z",
		"https://gofile.io/d/abcd",
		"https://mega.nz/folder/XyZw#k3y",
		"https://cyberfile.me/kwNx",
		"https://bunkr.ws/a/Q1w2",
		"yt-dlp:https://video.example/watch?v=1",
	}
	if got := ExtractLinks(text); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("ExtractLinks =\n %q\nwant\n %q", got, want)
	}
}

func TestIsHostCall(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"chrome-extension://abcdefghijklmnopabcdefghijklmnop/", "--parent-window=0"}, true},
		{[]string{`C:\Users\x\AppData\Roaming\Siphon\browser\` + HostName + ".firefox.json", FirefoxID}, true},
		{[]string{"/home/x/.mozilla/native-messaging-hosts/" + HostName + ".json", FirefoxID}, true},
		{[]string{"https://mega.nz/file/AbCd#key"}, false},
		{[]string{"doctor", "-v"}, false},
		{[]string{"x.json", "someone-else@example.com"}, false},
		{nil, false},
	} {
		if got := IsHostCall(c.args); got != c.want {
			t.Errorf("IsHostCall(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

func frame(b []byte) []byte {
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(b)))
	out.Write(b)
	return out.Bytes()
}

// Messages are a 4-byte length in native byte order, then JSON; every
// request gets one answer, until the browser closes the pipe.
func TestServe(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame([]byte(`{"type":"ping"}`)))
	in.Write(frame([]byte(`{"type":"add","links":["https://x.test/a"],"page":true,"referrer":"https://x.test/"}`)))
	in.Write(frame([]byte(`not json`)))
	var out bytes.Buffer
	var seen []Request
	err := Serve(&in, &out, func(r Request) Response {
		seen = append(seen, r)
		return Response{OK: true, Added: len(r.Links)}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[1].Type != "add" || !seen[1].Page || seen[1].Links[0] != "https://x.test/a" {
		t.Fatalf("requests = %+v", seen)
	}
	var replies []Response
	for out.Len() > 0 {
		var n uint32
		_ = binary.Read(&out, binary.LittleEndian, &n)
		var r Response
		if err := json.Unmarshal(out.Next(int(n)), &r); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, r)
	}
	if len(replies) != 3 || !replies[0].OK || replies[1].Added != 1 || replies[2].OK || replies[2].Error == "" {
		t.Errorf("replies = %+v", replies)
	}
}

// A message claiming to be huge is refused before anything is read into memory.
func TestServeRefusesHugeMessages(t *testing.T) {
	var in bytes.Buffer
	_ = binary.Write(&in, binary.LittleEndian, uint32(maxMessage+1))
	err := Serve(&in, &bytes.Buffer{}, func(Request) Response { t.Error("handled"); return Response{} })
	if err == nil {
		t.Fatal("a huge message was accepted")
	}
}
