package hardened

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Protocol (all requests need "Authorization: Bearer <key>"):
//
//	GET    /v1/info                  {"lock_days": N, "version": "..."}
//	GET    /v1/files/<name>[?asof=]  content (Range supported); HEAD for the size
//	PUT    /v1/files/<name>          store a new file (409 if it exists with other content)
//	DELETE /v1/files/<name>          delete (hidden until its retention ends)
//	GET    /v1/list/<dir>[?asof=]    JSON array of names below dir
//	POST   /v1/keep                  {"names": [...]} -> {"extended": n}
//
// asof is a unix time: files deleted after it are visible again (read-only
// point-in-time view).
//
// A key bound to a path (Limits.Path) sees only names below that path:
// everything else is 404, exactly as if it did not exist. Uploads beyond a
// key's quota or daily limit get 507, write requests beyond its rate limit
// 429 with Retry-After. Limits never delete or shorten anything.
//
// GET /metrics (Prometheus text format) needs "Authorization: Bearer
// <metrics token>" and is off without a token.

// Info is returned by GET /v1/info.
type Info struct {
	LockDays int    `json:"lock_days"`
	Version  string `json:"version"`
}

// KeepRequest is the body of POST /v1/keep.
type KeepRequest struct {
	Names []string `json:"names"`
}

type KeepResponse struct {
	Extended int `json:"extended"`
}

// MaxFileSize bounds a single upload (packs are ~16-24 MiB).
const MaxFileSize = 4 << 30

type Server struct {
	Store   *Store
	Keys    *Keys
	Log     *slog.Logger
	Version string
	// MetricsToken enables GET /metrics for requests that present it.
	MetricsToken string

	lim limiter
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", s.auth(s.handleInfo))
	mux.HandleFunc("GET /v1/files/{name...}", s.auth(s.handleGet))
	mux.HandleFunc("PUT /v1/files/{name...}", s.auth(s.handlePut))
	mux.HandleFunc("DELETE /v1/files/{name...}", s.auth(s.handleDelete))
	mux.HandleFunc("GET /v1/list/{dir...}", s.auth(s.handleList))
	mux.HandleFunc("POST /v1/keep", s.auth(s.handleKeep))
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	return mux
}

func (s *Server) auth(h func(http.ResponseWriter, *http.Request, Key)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		key, ok := s.Keys.Lookup(token)
		if !ok {
			s.Log.Warn("rejected request with invalid access key", "remote", r.RemoteAddr, "path", r.URL.Path)
			time.Sleep(500 * time.Millisecond) // slow down guessing
			http.Error(w, "invalid access key", http.StatusUnauthorized)
			return
		}
		h(w, r, key)
	}
}

// outside answers a name beyond the key's path like a missing file, so the
// key learns nothing about other repositories.
func (s *Server) outside(w http.ResponseWriter, r *http.Request, k Key, name string) bool {
	if k.Allows(name) {
		return false
	}
	s.lim.refuse(k.Name, "path")
	s.Log.Warn("refused name outside the key's path", "key", k.Name, "path", k.Path, "name", name, "remote", r.RemoteAddr)
	http.Error(w, ErrNotFound.Error(), http.StatusNotFound)
	return true
}

// limited applies the key's rate limit to a write request.
func (s *Server) limited(w http.ResponseWriter, r *http.Request, k Key) bool {
	wait, ok := s.lim.rate(k)
	if ok {
		return false
	}
	secs := int(wait/time.Second) + 1
	s.Log.Warn("rate limit of key reached", "key", k.Name, "limit_per_minute", k.Rate, "remote", r.RemoteAddr)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, "rate limit of this access key reached; retry later", http.StatusTooManyRequests)
	return true
}

func asOf(r *http.Request) int64 {
	t, _ := strconv.ParseInt(r.URL.Query().Get("asof"), 10, 64)
	return t
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, k Key, err error) {
	var le errLimit
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrExists):
		s.lim.refuse(k.Name, "exists")
		s.Log.Warn("refused to overwrite file", "name", r.PathValue("name"), "key", k.Name, "remote", r.RemoteAddr)
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrInvalidName):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.As(err, &le):
		s.Log.Warn("upload refused", "key", k.Name, "reason", le.msg, "remote", r.RemoteAddr)
		http.Error(w, le.msg, http.StatusInsufficientStorage)
	default:
		s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request, _ Key) {
	writeJSON(w, Info{LockDays: s.Store.LockDays(), Version: s.Version})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, k Key) {
	name := r.PathValue("name")
	if s.outside(w, r, k, name) {
		return
	}
	f, err := s.Store.Open(name, asOf(r))
	if err != nil {
		s.fail(w, r, k, err)
		return
	}
	defer f.Close()
	s.lim.count(k.Name, func(st *keyState) { st.reads++ })
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, k Key) {
	name := r.PathValue("name")
	if s.outside(w, r, k, name) {
		return
	}
	if r.URL.Query().Has("asof") {
		http.Error(w, "point-in-time view is read-only", http.StatusForbidden)
		return
	}
	if r.ContentLength < 0 || r.ContentLength > MaxFileSize {
		http.Error(w, "Content-Length required (max 4 GiB)", http.StatusRequestEntityTooLarge)
		return
	}
	if s.limited(w, r, k) {
		return
	}
	// Space is reserved from Content-Length before the body is read.
	done, err := s.lim.reserve(k, r.ContentLength, s.Store.Usage)
	if err != nil {
		s.fail(w, r, k, err)
		return
	}
	// A body shorter than Content-Length fails with io.ErrUnexpectedEOF, so
	// a broken upload never becomes a file.
	created, err := s.Store.SaveCreated(name, http.MaxBytesReader(w, r.Body, r.ContentLength))
	done(created && err == nil)
	if err != nil {
		s.fail(w, r, k, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, k Key) {
	name := r.PathValue("name")
	if s.outside(w, r, k, name) || s.limited(w, r, k) {
		return
	}
	if err := s.Store.Remove(name); err != nil {
		s.fail(w, r, k, err)
		return
	}
	if !isLockFile(name) {
		s.lim.count(k.Name, func(st *keyState) { st.deletes++ })
		s.Log.Info("file deleted (hidden until its retention ends)", "name", name, "key", k.Name, "remote", r.RemoteAddr)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, k Key) {
	dir := r.PathValue("dir")
	// A key bound to a path may list it, anything below it, and the folders
	// above it (which then show only its own files).
	above := k.Path != "" && strings.HasPrefix(k.Path, dir+"/")
	if !above && s.outside(w, r, k, dir) {
		return
	}
	names, err := s.Store.List(dir, asOf(r))
	if err != nil {
		s.fail(w, r, k, err)
		return
	}
	if above {
		vis := names[:0]
		for _, n := range names {
			if k.Allows(n) {
				vis = append(vis, n)
			}
		}
		names = vis
	}
	if names == nil {
		names = []string{}
	}
	s.lim.count(k.Name, func(st *keyState) { st.reads++ })
	writeJSON(w, names)
}

func (s *Server) handleKeep(w http.ResponseWriter, r *http.Request, k Key) {
	var req KeepRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, n := range req.Names {
		if s.outside(w, r, k, n) {
			return
		}
	}
	if s.limited(w, r, k) {
		return
	}
	n, err := s.Store.Keep(req.Names)
	s.lim.count(k.Name, func(st *keyState) { st.keeps++; st.extended += int64(n) })
	if err != nil {
		s.fail(w, r, k, err)
		return
	}
	writeJSON(w, KeepResponse{Extended: n})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.MetricsToken == "" {
		http.NotFound(w, r)
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.MetricsToken)) != 1 {
		s.Log.Warn("rejected metrics request with invalid token", "remote", r.RemoteAddr)
		time.Sleep(500 * time.Millisecond)
		http.Error(w, "invalid metrics token", http.StatusUnauthorized)
		return
	}
	keys, err := s.Keys.List()
	if err != nil {
		s.Log.Error("metrics", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.lim.writeMetrics(w, keys, s.Store.LockDays(), s.Store.Usage)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
