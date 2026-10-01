package hardened

import (
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
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", s.auth(s.handleInfo))
	mux.HandleFunc("GET /v1/files/{name...}", s.auth(s.handleGet))
	mux.HandleFunc("PUT /v1/files/{name...}", s.auth(s.handlePut))
	mux.HandleFunc("DELETE /v1/files/{name...}", s.auth(s.handleDelete))
	mux.HandleFunc("GET /v1/list/{dir...}", s.auth(s.handleList))
	mux.HandleFunc("POST /v1/keep", s.auth(s.handleKeep))
	return mux
}

func (s *Server) auth(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		key, ok := s.Keys.Check(token)
		if !ok {
			s.Log.Warn("rejected request with invalid access key", "remote", r.RemoteAddr, "path", r.URL.Path)
			time.Sleep(500 * time.Millisecond) // slow down guessing
			http.Error(w, "invalid access key", http.StatusUnauthorized)
			return
		}
		h(w, r, key)
	}
}

func asOf(r *http.Request) int64 {
	t, _ := strconv.ParseInt(r.URL.Query().Get("asof"), 10, 64)
	return t
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrExists):
		s.Log.Warn("refused to overwrite file", "name", r.PathValue("name"), "remote", r.RemoteAddr)
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrInvalidName):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request, _ string) {
	writeJSON(w, Info{LockDays: s.Store.LockDays(), Version: s.Version})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, _ string) {
	f, err := s.Store.Open(r.PathValue("name"), asOf(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, _ string) {
	if r.URL.Query().Has("asof") {
		http.Error(w, "point-in-time view is read-only", http.StatusForbidden)
		return
	}
	if r.ContentLength < 0 || r.ContentLength > MaxFileSize {
		http.Error(w, "Content-Length required (max 4 GiB)", http.StatusRequestEntityTooLarge)
		return
	}
	// A body shorter than Content-Length fails with io.ErrUnexpectedEOF, so
	// a broken upload never becomes a file.
	if err := s.Store.Save(r.PathValue("name"), http.MaxBytesReader(w, r.Body, r.ContentLength)); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, key string) {
	name := r.PathValue("name")
	if err := s.Store.Remove(name); err != nil {
		s.fail(w, r, err)
		return
	}
	if !isLockFile(name) {
		s.Log.Info("file deleted (hidden until its retention ends)", "name", name, "key", key, "remote", r.RemoteAddr)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, _ string) {
	names, err := s.Store.List(r.PathValue("dir"), asOf(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, names)
}

func (s *Server) handleKeep(w http.ResponseWriter, r *http.Request, _ string) {
	var req KeepRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	n, err := s.Store.Keep(req.Names)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, KeepResponse{Extended: n})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
