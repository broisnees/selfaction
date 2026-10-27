package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/broisnees/selfaction/internal/github"
)

// Server receives webhooks, owns the queue, and serves workers and log pages.
type Server struct {
	Store       *Store
	GitHub      *github.Client // reads workflow files, posts statuses, and lends its token to workers
	Secret      string         // webhook secret; deliveries without a valid signature are refused
	Workflows   string         // directory of workflow files in each repository
	AllowForks  bool           // run pull requests from forks (their code runs on your workers)
	ExternalURL string         // public base URL, used in status links
	WorkerToken string         // shared secret workers authenticate with
	CacheURL    string         // optional actions/cache server handed to workers
	PollFor     time.Duration  // how long a claim waits for work before answering 204; 0 = 25s
	Log         logrus.FieldLogger

	wake wakeup
}

// LostAfter is how long a running job may go without a heartbeat before it fails.
const LostAfter = 3 * time.Minute

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/github", s.webhook)
	mux.HandleFunc("POST /api/v1/claim", s.worker(s.claim))
	mux.HandleFunc("POST /api/v1/jobs/{id}/log", s.worker(s.appendLog))
	mux.HandleFunc("POST /api/v1/jobs/{id}/finish", s.worker(s.finish))
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.worker(s.cancel))
	mux.HandleFunc("GET /api/v1/jobs", s.worker(s.list))
	mux.HandleFunc("GET /jobs/{id}", s.page)
	mux.HandleFunc("GET /jobs/{id}/log", s.rawLog)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	return mux
}

// RunReaper fails jobs whose worker went silent, until ctx ends.
func (s *Server) RunReaper(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		lost, err := s.Store.Reap(ctx, LostAfter)
		if err != nil {
			s.Log.Warnf("reap: %v", err)
		}
		for _, j := range lost {
			s.Log.Warnf("job %s (%s %s): worker %s stopped responding", j.ID, j.Repo, j.Name, j.Worker)
			s.report(j)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// worker wraps handlers that only workers (holders of WorkerToken) may call.
func (s *Server) worker(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.WorkerToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.WorkerToken)) != 1 {
			http.Error(w, "invalid worker token", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) jobURL(id string) string {
	return strings.TrimSuffix(s.ExternalURL, "/") + "/jobs/" + id
}

// wakeup lets long-polling workers sleep until a job is queued.
type wakeup struct {
	mu sync.Mutex
	ch chan struct{}
}

func (w *wakeup) wait() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch == nil {
		w.ch = make(chan struct{})
	}
	return w.ch
}

func (w *wakeup) broadcast() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch != nil {
		close(w.ch)
		w.ch = nil
	}
}
