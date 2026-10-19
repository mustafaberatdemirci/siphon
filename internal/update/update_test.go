package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		current, candidate string
		want               bool
	}{
		{"v0.1.1", "v0.1.2", true},
		{"v0.1.2", "v0.1.2", false},
		{"v0.1.2", "v0.1.1", false},
		{"v0.1.9", "v0.2.0", true},
		{"v0.9.0", "v1.0.0", true},
		{"v0.1.10", "v0.1.9", false}, // numbers, not text
		{"v0.1.0-rc.1", "v0.1.0", true},
		{"v0.1.0-rc.1", "v0.1.0-rc.2", true},
		{"v0.1.0-rc.10", "v0.1.0-rc.9", false},
		{"v0.1.2-local+46137fd-dirty", "v0.1.2", false}, // a local build is its release
		{"v0.1.2-local+46137fd", "v0.1.3", true},
		{"dev", "v9.9.9", false},
		{"v0.1.2", "garbage", false},
	} {
		if got := Newer(tc.current, tc.candidate); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.current, tc.candidate, got, tc.want)
		}
	}
	if Comparable("dev") || !Comparable("v0.1.2-local+x") {
		t.Error("Comparable is wrong")
	}
}

func TestLatest(t *testing.T) {
	answer := `{"tag_name":"v0.1.3","html_url":"https://github.com/mustafaberatdemirci/siphon/releases/tag/v0.1.3","draft":false,"prerelease":false}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "GitHub wants a User-Agent", http.StatusForbidden)
			return
		}
		w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()

	r, err := Latest(ctx, srv.Client(), srv.URL)
	if err != nil || r.Version != "v0.1.3" || r.URL != "https://github.com/mustafaberatdemirci/siphon/releases/tag/v0.1.3" {
		t.Fatalf("got %+v %v", r, err)
	}

	// A page that isn't GitHub's isn't linked to.
	answer = `{"tag_name":"v0.1.3","html_url":"https://elsewhere.example/get-it"}`
	if r, err := Latest(ctx, srv.Client(), srv.URL); err != nil || r.URL != "https://github.com/mustafaberatdemirci/siphon/releases/latest" {
		t.Errorf("foreign page: %+v %v", r, err)
	}
	for _, bad := range []string{`{"tag_name":"nightly"}`, `{"tag_name":"v0.2.0","prerelease":true}`, `not json`} {
		answer = bad
		if _, err := Latest(ctx, srv.Client(), srv.URL); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}
