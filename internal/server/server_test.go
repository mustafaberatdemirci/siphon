package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
	"github.com/mustafaberatdemirci/siphon/internal/testutil"
)

type fakeEngine struct {
	mu        sync.Mutex
	jobs      []queue.Job
	added     []string // link|dir
	calls     []string
	maxActive int
	speed     int64
}

func (f *fakeEngine) Jobs() []queue.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]queue.Job(nil), f.jobs...)
}
func (f *fakeEngine) Add(_ context.Context, u, dir string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(u, "bad") {
		return 0, errors.New("no resolver")
	}
	f.added = append(f.added, u+"|"+dir)
	return 2, nil
}
func (f *fakeEngine) call(s string)    { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeEngine) Pause(id string)  { f.call("pause " + id) }
func (f *fakeEngine) Resume(id string) { f.call("resume " + id) }
func (f *fakeEngine) Remove(id string, del bool) {
	f.call("remove " + id + map[bool]string{true: " delete"}[del])
}
func (f *fakeEngine) PauseAll()               { f.call("pause all") }
func (f *fakeEngine) ResumeAll()              { f.call("resume all") }
func (f *fakeEngine) ClearFinished()          { f.call("clear finished") }
func (f *fakeEngine) RetryFailed()            { f.call("retry failed") }
func (f *fakeEngine) Paused() bool            { return false }
func (f *fakeEngine) MaxActive() int          { return f.maxActive }
func (f *fakeEngine) SetMaxActive(n int)      { f.maxActive = n }
func (f *fakeEngine) SpeedLimit() int64       { return f.speed }
func (f *fakeEngine) SetSpeedLimit(bps int64) { f.speed = bps }

const testToken = "s3cret-token"

func newTest(t *testing.T) (*Server, *fakeEngine, *httptest.Server, string) {
	t.Helper()
	eng := &fakeEngine{jobs: []queue.Job{{ID: "j1", Filename: "a.zip", State: queue.StateRunning, Size: 10, Done: 4}}, maxActive: 4}
	out := filepath.Join(testutil.TempDir(t), "downloads")
	srv, err := New(context.Background(), eng, testToken, out, "v-test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, eng, ts, out
}

func do(t *testing.T, ts *httptest.Server, method, path, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestNeedsAToken(t *testing.T) {
	if _, err := New(context.Background(), &fakeEngine{}, " ", "/x", ""); err == nil {
		t.Error("a server without a token was made")
	}
	_, eng, ts, _ := newTest(t)
	for _, tok := range []string{"", "wrong", testToken + "x"} {
		for _, c := range []struct{ method, path string }{
			{"GET", "/api/state"}, {"POST", "/api/add"}, {"POST", "/api/jobs/j1/pause"},
			{"POST", "/api/all/pause"}, {"POST", "/api/settings"},
		} {
			if resp, _ := do(t, ts, c.method, c.path, tok, `{"links":["x"]}`); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: %d", c.method, c.path, tok, resp.StatusCode)
			}
		}
	}
	if len(eng.calls) != 0 || len(eng.added) != 0 {
		t.Errorf("the engine was driven without the token: %v %v", eng.calls, eng.added)
	}
	// The page itself needs no token (it asks for it) and can't be framed.
	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("page: %d, CSP %q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
}

func TestStateAndActions(t *testing.T) {
	srv, eng, ts, _ := newTest(t)
	srv.Notify("hello")
	resp, st := do(t, ts, "GET", "/api/state", testToken, "")
	if resp.StatusCode != http.StatusOK || st["version"] != "v-test" || len(st["jobs"].([]any)) != 1 || len(st["notices"].([]any)) != 1 {
		t.Fatalf("state: %d %v", resp.StatusCode, st)
	}
	for path, want := range map[string]string{
		"/api/jobs/j1/pause":           "pause j1",
		"/api/jobs/j1/resume":          "resume j1",
		"/api/jobs/j1/remove?delete=1": "remove j1 delete",
		"/api/all/clear-finished":      "clear finished",
		"/api/all/retry-failed":        "retry failed",
	} {
		eng.calls = nil
		if resp, _ := do(t, ts, "POST", path, testToken, ""); resp.StatusCode != http.StatusOK || len(eng.calls) != 1 || eng.calls[0] != want {
			t.Errorf("%s: %d %v, want %q", path, resp.StatusCode, eng.calls, want)
		}
	}
	for _, path := range []string{"/api/jobs/nope/pause", "/api/jobs/j1/explode", "/api/all/explode"} {
		if resp, _ := do(t, ts, "POST", path, testToken, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, resp.StatusCode)
		}
	}
}

// Links go under the download folder, or a folder in it, never elsewhere;
// what came of them shows in the notices.
func TestAdd(t *testing.T) {
	srv, eng, ts, out := newTest(t)
	for _, bad := range []string{"../elsewhere", "/etc", "a/../../b", ".."} {
		if resp, _ := do(t, ts, "POST", "/api/add", testToken, `{"links":["https://x/1"],"folder":`+jsonString(bad)+`}`); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("folder %q: %d, want 400", bad, resp.StatusCode)
		}
	}
	if resp, _ := do(t, ts, "POST", "/api/add", testToken, `{"links":[]}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no links: %d", resp.StatusCode)
	}
	resp, body := do(t, ts, "POST", "/api/add", testToken, `{"links":["https://x/1\nhttps://x/2","https://bad/3"],"folder":"Albums/2026"}`)
	if resp.StatusCode != http.StatusAccepted || body["accepted"] != float64(3) {
		t.Fatalf("add: %d %v", resp.StatusCode, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.mu.Lock()
		adding, n := srv.adding, len(srv.notices)
		srv.mu.Unlock()
		if adding == 0 && n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the links were never added")
		}
		time.Sleep(10 * time.Millisecond)
	}
	dir := filepath.Join(out, "Albums", "2026")
	if strings.Join(eng.added, " ") != "https://x/1|"+dir+" https://x/2|"+dir {
		t.Errorf("added: %v", eng.added)
	}
	srv.mu.Lock()
	notes := srv.notices
	srv.mu.Unlock()
	last := notes[len(notes)-1].Text
	if last != "Files added to the queue: 4. Links that could not be added: 1." || !strings.Contains(notes[0].Text, "https://bad/3") {
		t.Errorf("notices: %v", notes)
	}
}

func TestSettings(t *testing.T) {
	_, eng, ts, _ := newTest(t)
	if resp, _ := do(t, ts, "POST", "/api/settings", testToken, `{"max_active":6,"speed_limit":1048576}`); resp.StatusCode != http.StatusOK || eng.maxActive != 6 || eng.speed != 1048576 {
		t.Errorf("settings: %d, %d, %d", resp.StatusCode, eng.maxActive, eng.speed)
	}
	for _, bad := range []string{`{"max_active":0}`, `{"max_active":99}`, `{"speed_limit":-1}`, `nope`} {
		if resp, _ := do(t, ts, "POST", "/api/settings", testToken, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, resp.StatusCode)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
