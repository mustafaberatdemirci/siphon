// Package update tells whether a newer Siphon has been released.
//
// It asks GitHub for the latest release, one small request without any
// identifier of the user, and compares versions. Nothing is downloaded or
// installed: the window version only says a new version is out and links to
// its page. An installer that replaces itself would be the one program
// worth attacking, and Siphon isn't signed yet.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// LatestURL is the GitHub API address of the latest release (drafts and
// pre-releases left out by GitHub).
const LatestURL = "https://api.github.com/repos/mustafaberatdemirci/siphon/releases/latest"

// Release is a published version.
type Release struct {
	Version string // "v0.1.2"
	URL     string // its page
}

// Latest asks for the latest release.
func Latest(ctx context.Context, client *http.Client, endpoint string) (Release, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "siphon-update-check")
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("%s: HTTP %s", endpoint, resp.Status)
	}
	var r struct {
		TagName    string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return Release{}, fmt.Errorf("release answer: %w", err)
	}
	if _, ok := parse(r.TagName); !ok || r.Draft || r.Prerelease {
		return Release{}, fmt.Errorf("release answer: no released version (%q)", r.TagName)
	}
	// The page is GitHub's own; never a link from elsewhere.
	if !strings.HasPrefix(r.HTMLURL, "https://github.com/") {
		r.HTMLURL = "https://github.com/mustafaberatdemirci/siphon/releases/latest"
	}
	return Release{Version: r.TagName, URL: r.HTMLURL}, nil
}

// Comparable reports whether current is a version a release can be compared
// with: "dev" (a build without a version) isn't.
func Comparable(current string) bool {
	_, ok := parse(current)
	return ok
}

// Newer reports whether candidate is a newer version than current. A local
// build ("v0.1.2-local+abc") counts as its release: it is the same code or
// newer, and only a later release is news. A release candidate is older
// than its release.
func Newer(current, candidate string) bool {
	c, ok1 := parse(current)
	n, ok2 := parse(candidate)
	if !ok1 || !ok2 {
		return false
	}
	return compare(n, c) > 0
}

type version struct {
	core [3]int
	pre  []string // dot-separated pre-release identifiers; nil for a release
}

// parse reads "v1.2.3", "1.2.3-rc.1", "v1.2.3+build". A local build's
// pre-release ("-local…") is dropped (see Newer).
func parse(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v version
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return v, false
		}
		v.core[i] = n
	}
	if hasPre && !strings.HasPrefix(pre, "local") {
		if pre == "" {
			return v, false
		}
		v.pre = strings.Split(pre, ".")
	}
	return v, true
}

// compare is semantic versioning's order: core first, then a pre-release
// before its release, then identifier by identifier.
func compare(a, b version) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			if a.core[i] > b.core[i] {
				return 1
			}
			return -1
		}
	}
	switch {
	case a.pre == nil && b.pre == nil:
		return 0
	case a.pre == nil:
		return 1
	case b.pre == nil:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := compareIdent(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) > len(b.pre):
		return 1
	case len(a.pre) < len(b.pre):
		return -1
	}
	return 0
}

func compareIdent(a, b string) int {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		switch {
		case na > nb:
			return 1
		case na < nb:
			return -1
		}
		return 0
	case ea == nil: // numbers sort before words
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}
