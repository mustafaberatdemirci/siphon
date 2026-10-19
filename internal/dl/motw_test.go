package dl

import "testing"

func TestWebAddressKeepsNoSecrets(t *testing.T) {
	for in, want := range map[string]string{
		"https://mega.nz/file/AbCd#k3y-in-the-fragment": "https://mega.nz/file/AbCd",
		"https://gofile.io/d/abcd?password=0123abcd":    "https://gofile.io/d/abcd",
		"yt-dlp:https://video.example/watch?v=1":        "https://video.example/watch",
		"https://user:secret@files.example/a.zip":       "https://files.example/a.zip",
		"http://plain.example/x":                        "http://plain.example/x",
		"file://a.bin":                                  "",
		"":                                              "",
	} {
		if got := webAddress(in); got != want {
			t.Errorf("webAddress(%q) = %q, want %q", in, got, want)
		}
	}
}
