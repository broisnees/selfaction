package cache

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Server serves the actions/cache v1 API under /{token}/{scope}/_apis/artifactcache.
// Jobs get ACTIONS_CACHE_URL=<base>/<token>/<scope>/, so the token never has to be
// passed as a header and each repository gets its own namespace.
type Server struct {
	Store       *Store
	Token       string
	ExternalURL string // public base URL for download links; empty = derive from the request
	Log         logrus.FieldLogger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	base := "/{token}/{scope}/_apis/artifactcache"
	mux.HandleFunc("GET "+base+"/cache", s.auth(s.find))
	mux.HandleFunc("POST "+base+"/caches", s.auth(s.reserve))
	mux.HandleFunc("PATCH "+base+"/caches/{id}", s.auth(s.upload))
	mux.HandleFunc("POST "+base+"/caches/{id}", s.auth(s.commit))
	mux.HandleFunc("GET "+base+"/artifacts/{id}", s.auth(s.download))
	mux.HandleFunc("POST "+base+"/clean", s.auth(func(w http.ResponseWriter, r *http.Request, _ string) { reply(w, 200, struct{}{}) }))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]string{"status": "ok"}) })
	return mux
}

type scopedHandler func(w http.ResponseWriter, r *http.Request, scope string)

func (s *Server) auth(h scopedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.PathValue("token")), []byte(s.Token)) != 1 {
			reply(w, 401, errors.New("invalid cache token"))
			return
		}
		scope := r.PathValue("scope")
		if !ValidScope(scope) {
			reply(w, 400, fmt.Errorf("invalid scope %q", scope))
			return
		}
		s.Log.Debugf("%s %s", r.Method, r.URL.Path)
		h(w, r, scope)
	}
}

func (s *Server) find(w http.ResponseWriter, r *http.Request, scope string) {
	keys := strings.Split(r.URL.Query().Get("keys"), ",")
	e, err := s.Store.Find(r.Context(), scope, keys, r.URL.Query().Get("version"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if e == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	reply(w, 200, map[string]any{
		"result":          "hit",
		"scope":           scope,
		"cacheKey":        e.Key,
		"cacheVersion":    e.Version,
		"creationTime":    e.CreatedAt.UTC().Format(time.RFC3339),
		"archiveLocation": fmt.Sprintf("%s/%s/%s/_apis/artifactcache/artifacts/%d", s.baseURL(r), url.PathEscape(s.Token), scope, e.ID),
	})
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request, scope string) {
	var req struct {
		Key     string `json:"key"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		reply(w, 400, err)
		return
	}
	if req.Key == "" || req.Version == "" || len(req.Key) > 512 {
		reply(w, 400, errors.New("key (max 512 chars) and version are required"))
		return
	}
	id, err := s.Store.Reserve(r.Context(), scope, req.Key, req.Version)
	if errors.Is(err, ErrExists) {
		reply(w, 409, err)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	reply(w, 200, map[string]int64{"cacheId": id})
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, scope string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		reply(w, 400, err)
		return
	}
	start, end, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		reply(w, 400, err)
		return
	}
	n, err := s.Store.Write(r.Context(), scope, id, start, io.LimitReader(r.Body, end-start+1))
	if errors.Is(err, ErrNotFound) {
		reply(w, 404, err)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if n != end-start+1 {
		reply(w, 400, fmt.Errorf("chunk %d-%d: got %d bytes", start, end, n))
		return
	}
	reply(w, 200, struct{}{})
}

func (s *Server) commit(w http.ResponseWriter, r *http.Request, scope string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		reply(w, 400, err)
		return
	}
	var req struct {
		Size int64 `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		reply(w, 400, err)
		return
	}
	switch err := s.Store.Commit(r.Context(), scope, id, req.Size); {
	case errors.Is(err, ErrNotFound):
		reply(w, 404, err)
	case err != nil:
		reply(w, 400, err)
	default:
		reply(w, 200, struct{}{})
	}
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, scope string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		reply(w, 400, err)
		return
	}
	f, e, err := s.Store.OpenBlob(r.Context(), scope, id)
	if errors.Is(err, ErrNotFound) {
		reply(w, 404, err)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", e.CreatedAt, f)
}

func (s *Server) baseURL(r *http.Request) string {
	if s.ExternalURL != "" {
		return strings.TrimSuffix(s.ExternalURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Errorf("cache: %v", err)
	reply(w, 500, err)
}

func reply(w http.ResponseWriter, code int, v any) {
	if err, ok := v.(error); ok {
		v = map[string]string{"message": err.Error()}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// parseContentRange reads "bytes <start>-<end>/*", the only form actions/cache sends.
func parseContentRange(h string) (int64, int64, error) {
	rng, _, _ := strings.Cut(strings.TrimPrefix(h, "bytes "), "/")
	a, b, ok := strings.Cut(rng, "-")
	start, err1 := strconv.ParseInt(a, 10, 64)
	end, err2 := strconv.ParseInt(b, 10, 64)
	if !ok || err1 != nil || err2 != nil || start < 0 || end < start {
		return 0, 0, fmt.Errorf("invalid Content-Range %q", h)
	}
	return start, end, nil
}
