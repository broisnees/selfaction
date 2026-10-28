package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/broisnees/selfaction/internal/github"
)

// Server receives webhooks, owns the queue, and serves workers, artifacts and pages.
type Server struct {
	Store             *Store
	GitHub            *github.Client // with a token: reads workflow files, posts statuses and lends its token to workers
	App               *github.App    // optional GitHub App: per-installation tokens, and check runs instead of statuses
	Secret            string         // webhook secret; deliveries without a valid signature are refused
	Workflows         string         // directory of workflow files in each repository
	AllowForks        bool           // run pull requests from forks (their code runs on your workers)
	ExternalURL       string         // public base URL, used in status links and for the artifact service
	WorkerToken       string         // shared secret workers authenticate with
	CacheURL          string         // optional actions/cache server handed to workers
	Sealer            *Sealer        // encrypts repository secrets at rest; nil turns secrets off
	Artifacts         string         // directory for artifacts jobs upload; "" turns the artifact service off
	ArtifactRetention time.Duration  // longest an artifact is kept; 0 = 90 days
	PollFor           time.Duration  // how long a claim waits for work before answering 204; 0 = 25s
	Log               logrus.FieldLogger

	wake     wakeup
	progress sync.Mutex // one run advances at a time, so two finishing jobs can't release a job twice
	reports  reporter
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
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", s.worker(s.cancelRun))
	mux.HandleFunc("GET /api/v1/jobs", s.worker(s.list))
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/secrets", s.worker(s.listSecrets))
	mux.HandleFunc("PUT /api/v1/repos/{owner}/{repo}/secrets/{name}", s.worker(s.putSecret))
	mux.HandleFunc("DELETE /api/v1/repos/{owner}/{repo}/secrets/{name}", s.worker(s.deleteSecret))
	mux.HandleFunc("GET /jobs/{id}", s.page)
	mux.HandleFunc("GET /jobs/{id}/log", s.rawLog)
	mux.HandleFunc("GET /runs/{id}", s.runPage)
	mux.HandleFunc("GET /runs/{id}/artifacts/{name}", s.downloadRunArtifact)
	if s.Artifacts != "" {
		mux.HandleFunc("POST "+artifactService+"/{method}", s.artifactRPC)
		mux.HandleFunc("PUT /artifacts/blob/{id}", s.uploadBlob)
		mux.HandleFunc("GET /artifacts/blob/{id}", s.downloadBlob)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	return mux
}

// RunReaper fails jobs whose worker went silent and expires old artifacts, until ctx ends.
func (s *Server) RunReaper(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	lastGC := time.Time{}
	for {
		lost, err := s.Store.Reap(ctx, LostAfter)
		if err != nil {
			s.Log.Warnf("reap: %v", err)
		}
		for _, j := range lost {
			s.Log.Warnf("job %s (%s %s / %s): worker %s stopped responding", j.ID, j.Repo, j.WorkflowName, j.Name, j.Worker)
			s.report(j)
			s.advance(ctx, j.RunID)
		}
		// Runs whose waiting jobs nothing will release any more, e.g. after a
		// restart between a job finishing and its run moving on.
		if stalled, err := s.Store.StalledRuns(ctx); err != nil {
			s.Log.Warnf("stalled runs: %v", err)
		} else {
			for _, id := range stalled {
				s.advance(ctx, id)
			}
		}
		if s.Artifacts != "" && time.Since(lastGC) > 10*time.Minute {
			lastGC = time.Now()
			if n, err := s.expireArtifacts(ctx); err != nil {
				s.Log.Warnf("artifact gc: %v", err)
			} else if n > 0 {
				s.Log.Infof("artifact gc: removed %d artifacts", n)
			}
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

func (s *Server) base() string { return strings.TrimSuffix(s.ExternalURL, "/") }

func (s *Server) jobURL(id string) string { return s.base() + "/jobs/" + id }

func (s *Server) runURL(id string) string { return s.base() + "/runs/" + id }

// signingKey signs artifact tokens and download links. It derives from the
// worker token, which workers hold anyway.
func (s *Server) signingKey() []byte {
	k := sha256.Sum256([]byte("selfaction artifacts\x00" + s.WorkerToken))
	return k[:]
}

// client is the GitHub API client for a run: the server's token, or a token
// for the run's GitHub App installation.
func (s *Server) client(ctx context.Context, installation int64) (*github.Client, error) {
	if s.App == nil {
		return s.GitHub, nil
	}
	if installation == 0 {
		return nil, errors.New("the delivery names no GitHub App installation")
	}
	token, err := s.App.Token(ctx, installation)
	if err != nil {
		return nil, err
	}
	return &github.Client{API: s.App.API, Token: token, HTTP: s.App.HTTP}, nil
}

// startRun stores a new run with its first jobs and releases what can start.
func (s *Server) startRun(ctx context.Context, run *Run) error {
	s.progress.Lock()
	defer s.progress.Unlock()
	c, err := progress(run, nil)
	if err != nil {
		return err
	}
	if err := s.Store.CreateRun(ctx, run, c.added); err != nil {
		return err
	}
	return s.apply(ctx, run, c)
}

// advance moves a run on after one of its jobs changed: it releases jobs
// whose needs are done, skips the ones that can no longer run, and applies
// fail-fast.
func (s *Server) advance(ctx context.Context, runID string) {
	s.progress.Lock()
	defer s.progress.Unlock()
	if err := s.advanceLocked(ctx, runID); err != nil {
		s.Log.Warnf("run %s: %v", runID, err)
	}
}

func (s *Server) advanceLocked(ctx context.Context, runID string) error {
	run, err := s.Store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	rows, err := s.Store.Jobs(ctx, runID)
	if err != nil {
		return err
	}
	c, err := progress(run, rows)
	if err != nil {
		return err
	}
	if err := s.Store.Save(ctx, c.changed, c.added); err != nil {
		return err
	}
	return s.apply(ctx, run, c)
}

// apply logs, reports and cancels what progress decided, after it was saved.
func (s *Server) apply(ctx context.Context, run *Run, c *change) error {
	queued := false
	for _, j := range append(c.added, c.changed...) {
		for _, m := range c.messages[j] {
			_ = s.Store.AppendLog(j.ID, []byte(m+"\n"))
		}
		if j.Status == Queued {
			queued = true
			s.Log.Infof("queued %s / %s for %s %s@%.7s (job %s)", run.Name, j.Name, run.Event, run.Repo, run.SHA, j.ID)
		}
		s.report(j)
	}
	for _, j := range c.cancel {
		got, err := s.Store.Cancel(ctx, j.ID)
		if err != nil {
			return err
		}
		_ = s.Store.AppendLog(j.ID, []byte("\nCancelled because another combination of "+j.Key+" failed (strategy.fail-fast).\n"))
		s.report(got)
	}
	if queued {
		s.wake.broadcast()
	}
	if len(c.cancel) > 0 {
		return s.advanceLocked(ctx, run.ID) // the cancelled jobs may complete their job's result
	}
	return nil
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
