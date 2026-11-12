// Package server is Siphon's server mode ("siphon serve"): the download
// queue the window has, driven from a browser instead, for a machine
// without a screen (a home server, a NAS, a container).
//
// It serves one page (web/index.html, embedded) and a small JSON API under
// /api/. Every API call needs the token, sent as "Authorization: Bearer
// <token>": the queue writes files and starts downloads, so nobody else on
// the network may drive it. The token travels in a header, never in a
// cookie, so another site open in the same browser can't make the browser
// send it (no cross-site request forgery).
//
// Links are saved under the output folder only: a request may name a
// subfolder in it, never a path outside.
package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mustafaberatdemirci/siphon/internal/queue"
)

//go:embed web
var webFiles embed.FS

// Engine is what the server drives: queue.Engine's methods it needs, so a
// test can stand in for it.
type Engine interface {
	Jobs() []queue.Job
	Add(ctx context.Context, rawURL, outDir string) (int, error)
	Pause(id string)
	Resume(id string)
	Remove(id string, deleteFiles bool)
	PauseAll()
	ResumeAll()
	ClearFinished()
	RetryFailed()
	Paused() bool
	MaxActive() int
	SetMaxActive(n int)
	SpeedLimit() int64
	SetSpeedLimit(bytesPerSec int64)
}

// Server is the HTTP side of server mode.
type Server struct {
	eng     Engine
	token   string
	outDir  string
	version string

	// addCtx bounds the background resolution of added links; it ends
	// when the server stops.
	addCtx context.Context

	mu      sync.Mutex
	notices []Notice
	adding  int // links still being resolved
}

// Notice is a line for the page's status area.
type Notice struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// maxNotices bounds the notices kept.
const maxNotices = 50

// New makes a server for eng. token must not be empty. outDir is where
// links are saved. ctx ends the resolution of links still being added.
func New(ctx context.Context, eng Engine, token, outDir, version string) (*Server, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("server mode needs a token")
	}
	return &Server{eng: eng, token: token, outDir: outDir, version: version, addCtx: ctx}, nil
}

// Notify adds a line to the page's status area (the engine's notices go
// here too).
func (s *Server) Notify(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notices = append(s.notices, Notice{At: time.Now(), Text: text})
	if len(s.notices) > maxNotices {
		s.notices = s.notices[len(s.notices)-maxNotices:]
	}
}

// Handler is the whole site: the page and the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	page, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /", http.FileServerFS(page))
	mux.HandleFunc("GET /api/state", s.auth(s.state))
	mux.HandleFunc("POST /api/add", s.auth(s.add))
	mux.HandleFunc("POST /api/jobs/{id}/{action}", s.auth(s.job))
	mux.HandleFunc("POST /api/all/{action}", s.auth(s.all))
	mux.HandleFunc("POST /api/settings", s.auth(s.settings))
	return securityHeaders(mux)
}

// securityHeaders: the page loads nothing from elsewhere and may not be
// framed by another site.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "a valid token is needed")
			return
		}
		next(w, r)
	}
}

// stateReply is GET /api/state: everything the page shows.
type stateReply struct {
	Version    string      `json:"version"`
	Jobs       []queue.Job `json:"jobs"`
	Paused     bool        `json:"paused"`
	MaxActive  int         `json:"max_active"`
	SpeedLimit int64       `json:"speed_limit"`
	Adding     int         `json:"adding"`
	Notices    []Notice    `json:"notices"`
}

func (s *Server) state(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	notices := append([]Notice(nil), s.notices...)
	adding := s.adding
	s.mu.Unlock()
	jobs := s.eng.Jobs()
	if jobs == nil {
		jobs = []queue.Job{}
	}
	writeJSON(w, http.StatusOK, stateReply{
		Version: s.version, Jobs: jobs, Paused: s.eng.Paused(),
		MaxActive: s.eng.MaxActive(), SpeedLimit: s.eng.SpeedLimit(),
		Adding: adding, Notices: notices,
	})
}

// addRequest is POST /api/add: links, one per entry or several per line,
// and an optional subfolder of the output folder.
type addRequest struct {
	Links  []string `json:"links"`
	Folder string   `json:"folder"`
}

// maxLinks bounds one request.
const maxLinks = 10000

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	var req addRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "the request isn't JSON with a links list")
		return
	}
	var links []string
	for _, l := range req.Links {
		for _, f := range strings.Fields(l) {
			links = append(links, f)
		}
	}
	if len(links) == 0 || len(links) > maxLinks {
		writeError(w, http.StatusBadRequest, "give between 1 and 10000 links")
		return
	}
	dir, err := s.folder(req.Folder)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	s.adding += len(links)
	s.mu.Unlock()
	// Resolving an album can take minutes: the page is told the links were
	// taken, and the notices say what came of them.
	go s.addAll(links, dir)
	writeJSON(w, http.StatusAccepted, map[string]int{"accepted": len(links)})
}

// folder is where a request's links go: the output folder, or a folder
// inside it. A path that would leave it is refused.
func (s *Server) folder(sub string) (string, error) {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return s.outDir, nil
	}
	clean := filepath.FromSlash(sub)
	if !filepath.IsLocal(clean) {
		return "", errors.New("the folder must be a folder inside the download folder")
	}
	return filepath.Join(s.outDir, clean), nil
}

func (s *Server) addAll(links []string, dir string) {
	files, failed := 0, 0
	for _, l := range links {
		n, err := s.eng.Add(s.addCtx, l, dir)
		s.mu.Lock()
		s.adding--
		s.mu.Unlock()
		if err != nil {
			if s.addCtx.Err() != nil {
				return
			}
			failed++
			s.Notify("Could not add " + l + ": " + err.Error())
			continue
		}
		files += n
	}
	msg := "Files added to the queue: " + strconv.Itoa(files) + "."
	if failed > 0 {
		msg += " Links that could not be added: " + strconv.Itoa(failed) + "."
	}
	s.Notify(msg)
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	known := false
	for _, j := range s.eng.Jobs() {
		if j.ID == id {
			known = true
			break
		}
	}
	if !known {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	switch r.PathValue("action") {
	case "pause":
		s.eng.Pause(id)
	case "resume":
		s.eng.Resume(id)
	case "remove":
		s.eng.Remove(id, r.URL.Query().Get("delete") == "1")
	default:
		writeError(w, http.StatusNotFound, "no such action")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) all(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("action") {
	case "pause":
		s.eng.PauseAll()
	case "resume":
		s.eng.ResumeAll()
	case "clear-finished":
		s.eng.ClearFinished()
	case "retry-failed":
		s.eng.RetryFailed()
	default:
		writeError(w, http.StatusNotFound, "no such action")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// settingsRequest is POST /api/settings; a field left out stays as it is.
type settingsRequest struct {
	MaxActive  *int   `json:"max_active"`
	SpeedLimit *int64 `json:"speed_limit"` // bytes a second, 0 = none
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "the request isn't JSON")
		return
	}
	if req.MaxActive != nil {
		if *req.MaxActive < 1 || *req.MaxActive > 16 {
			writeError(w, http.StatusBadRequest, "downloads at once must be 1 to 16")
			return
		}
		s.eng.SetMaxActive(*req.MaxActive)
	}
	if req.SpeedLimit != nil {
		if *req.SpeedLimit < 0 {
			writeError(w, http.StatusBadRequest, "the speed limit can't be negative")
			return
		}
		s.eng.SetSpeedLimit(*req.SpeedLimit)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
