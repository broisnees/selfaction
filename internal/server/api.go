package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/nektos/act/pkg/model"
)

// Claim is what a worker receives with a job.
type Claim struct {
	Job      *Job              `json:"job"`
	Event    json.RawMessage   `json:"event"`     // the webhook payload, for github.event
	CloneURL string            `json:"clone_url"` // where to fetch the commit
	Token    string            `json:"token"`     // GitHub token for cloning and as the job's GITHUB_TOKEN
	CacheURL string            `json:"cache_url"` // actions/cache server, if the server offers one
	Needs    map[string]Need   `json:"needs"`     // results and outputs of the jobs this one needs, directly or not
	Secrets  map[string]string `json:"secrets,omitempty"`
	Env      map[string]string `json:"env,omitempty"` // run ids and the artifact service
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Worker string   `json:"worker"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Worker == "" {
		http.Error(w, "need a worker name and labels", http.StatusBadRequest)
		return
	}
	wait := s.PollFor
	if wait == 0 {
		wait = 25 * time.Second
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		wake := s.wake.wait()
		j, err := s.Store.Claim(r.Context(), req.Worker, req.Labels)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if j != nil {
			c, err := s.claimFor(r.Context(), j)
			if err != nil {
				// The job can't run anywhere if the server can't describe it; don't hand it out again.
				s.Log.Errorf("job %s: %v", j.ID, err)
				_ = s.Store.AppendLog(j.ID, []byte("The server could not start this job: "+err.Error()+"\n"))
				if j, err := s.Store.End(context.WithoutCancel(r.Context()), j.ID, Errored); err == nil {
					s.report(j)
					go s.advance(context.Background(), j.RunID)
				}
				continue
			}
			s.Log.Infof("job %s (%s %s / %s) claimed by %s", j.ID, j.Repo, j.WorkflowName, j.Name, j.Worker)
			s.report(j)
			writeJSON(w, c)
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// claimFor gathers what a worker needs to run j.
func (s *Server) claimFor(ctx context.Context, j *Job) (*Claim, error) {
	run, err := s.Store.GetRun(ctx, j.RunID)
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.Jobs(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	wf, err := model.ReadWorkflow(bytes.NewReader(run.Source), false)
	if err != nil {
		return nil, err
	}
	gh, err := s.client(ctx, run.Installation)
	if err != nil {
		return nil, err
	}
	c := &Claim{
		Job:      j,
		Event:    run.Payload,
		CloneURL: run.CloneURL,
		Token:    gh.Token,
		CacheURL: s.CacheURL,
		Needs:    needsFor(wf, j.Key, rows),
		Env: map[string]string{
			"GITHUB_RUN_ID":      strconv.FormatInt(run.Number, 10),
			"GITHUB_RUN_NUMBER":  strconv.FormatInt(run.RunNumber, 10),
			"GITHUB_RUN_ATTEMPT": "1",
		},
	}
	if s.Sealer != nil && !run.Fork {
		if c.Secrets, err = s.repoSecrets(ctx, run.Repo); err != nil {
			return nil, fmt.Errorf("read secrets: %w", err)
		}
	}
	if s.Artifacts != "" {
		// @actions/artifact only uses the origin of this URL.
		c.Env["ACTIONS_RUNTIME_URL"] = s.base() + "/"
		c.Env["ACTIONS_RESULTS_URL"] = s.base() + "/"
		c.Env["ACTIONS_RUNTIME_TOKEN"] = s.runtimeToken(run.ID, j.ID)
	}
	return c, nil
}

// appendLog stores a chunk of output and doubles as the heartbeat. It answers
// with the job's status; anything but "running" tells the worker to stop.
func (s *Server) appendLog(w http.ResponseWriter, r *http.Request) {
	id, worker := r.PathValue("id"), r.URL.Query().Get("worker")
	j, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if j.Worker != worker {
		http.Error(w, "job belongs to another worker", http.StatusConflict)
		return
	}
	status, err := s.Store.Heartbeat(r.Context(), id, worker)
	if err == nil && status == Running {
		var chunk []byte
		if chunk, err = io.ReadAll(io.LimitReader(r.Body, 4<<20)); err == nil && len(chunk) > 0 {
			err = s.Store.AppendLog(id, chunk)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": status})
}

func (s *Server) finish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Worker  string            `json:"worker"`
		Result  string            `json:"result"`
		Outputs map[string]string `json:"outputs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	j, err := s.Store.Finish(r.Context(), r.PathValue("id"), req.Worker, req.Result, req.Outputs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.Log.Infof("job %s (%s %s / %s): %s", j.ID, j.Repo, j.WorkflowName, j.Name, j.Status)
	s.report(j)
	s.advance(context.WithoutCancel(r.Context()), j.RunID) // the run must move on even if the worker hangs up
	writeJSON(w, j)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	j, err := s.Store.Cancel(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if j.Status == Cancelled {
		s.report(j)
		s.advance(context.WithoutCancel(r.Context()), j.RunID)
	}
	writeJSON(w, j)
}

// cancelRun cancels every job of a run that has not finished.
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jobs, err := s.Store.Jobs(r.Context(), run.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i, j := range jobs {
		if j.Done() {
			continue
		}
		if jobs[i], err = s.Store.Cancel(r.Context(), j.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.report(jobs[i])
	}
	s.advance(context.WithoutCancel(r.Context()), run.ID)
	writeJSON(w, jobs)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.List(r.Context(), 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, jobs)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
