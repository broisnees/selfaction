package server

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// report sends j's state to GitHub in the background: as a commit status, or
// as a check run when the server is a GitHub App. Both show on the commit and
// on pull requests. Updates for one job are sent in order, and a burst of
// them collapses into its latest state.
func (s *Server) report(j *Job) {
	if j == nil || j.Expand {
		return // a placeholder for matrix combinations not known yet
	}
	s.reports.push(s, j)
}

type reporter struct {
	mu      sync.Mutex
	pending map[string]*Job // newest unsent state per job
	busy    map[string]bool // jobs with a sender running
	checks  map[string]int64
}

func (r *reporter) push(s *Server, j *Job) {
	c := *j
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending, r.busy, r.checks = map[string]*Job{}, map[string]bool{}, map[string]int64{}
	}
	r.pending[j.ID] = &c
	if !r.busy[j.ID] {
		r.busy[j.ID] = true
		go r.drain(s, j.ID)
	}
}

func (r *reporter) drain(s *Server, id string) {
	for {
		r.mu.Lock()
		j := r.pending[id]
		delete(r.pending, id)
		if j == nil {
			delete(r.busy, id)
			r.mu.Unlock()
			return
		}
		check := r.checks[id]
		r.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var err error
		if s.App != nil {
			check, err = s.sendCheck(ctx, j, check)
			r.mu.Lock()
			if j.Done() {
				delete(r.checks, id)
			} else if check != 0 {
				r.checks[id] = check
			}
			r.mu.Unlock()
		} else {
			err = s.sendStatus(ctx, j)
		}
		cancel()
		if err != nil {
			s.Log.Warnf("report %s / %s on %s@%.7s: %v", j.WorkflowName, j.Name, j.Repo, j.SHA, err)
		}
	}
}

// checkName names a job's status or check run, like GitHub's own "ci / test (push)".
func checkName(j *Job) string {
	return fmt.Sprintf("%s / %s (%s)", j.WorkflowName, j.Name, j.Event)
}

func (s *Server) sendStatus(ctx context.Context, j *Job) error {
	gh, err := s.client(ctx, j.Installation)
	if err != nil {
		return err
	}
	state, desc := statusOf(j)
	return gh.Do(ctx, "POST", fmt.Sprintf("/repos/%s/statuses/%s", j.Repo, j.SHA), map[string]string{
		"state":       state,
		"description": desc,
		"context":     "selfaction/" + checkName(j),
		"target_url":  s.jobURL(j.ID),
	}, nil)
}

// sendCheck creates or updates j's check run and returns its id.
func (s *Server) sendCheck(ctx context.Context, j *Job, id int64) (int64, error) {
	gh, err := s.client(ctx, j.Installation)
	if err != nil {
		return id, err
	}
	if id == 0 {
		id = j.CheckRun
	}
	if id == 0 {
		if latest, err := s.Store.Get(ctx, j.ID); err == nil {
			id = latest.CheckRun
		}
	}
	_, desc := statusOf(j)
	body := map[string]any{
		"name":        checkName(j),
		"details_url": s.jobURL(j.ID),
		"external_id": j.ID,
		"output":      map[string]string{"title": desc, "summary": desc + ". Log: " + s.jobURL(j.ID)},
	}
	switch j.Status {
	case Waiting, Queued:
		body["status"] = "queued"
	case Running:
		body["status"] = "in_progress"
		body["started_at"] = j.StartedAt.UTC().Format(time.RFC3339)
	default:
		body["status"] = "completed"
		body["conclusion"] = map[string]string{Success: "success", Failure: "failure", Cancelled: "cancelled", Skipped: "skipped", Errored: "failure"}[j.Status]
		finished := j.FinishedAt
		if finished.IsZero() {
			finished = time.Now()
		}
		body["completed_at"] = finished.UTC().Format(time.RFC3339)
		if !j.StartedAt.IsZero() {
			body["started_at"] = j.StartedAt.UTC().Format(time.RFC3339)
		}
	}
	if id != 0 {
		return id, gh.Do(ctx, "PATCH", fmt.Sprintf("/repos/%s/check-runs/%d", j.Repo, id), body, nil)
	}
	body["head_sha"] = j.SHA
	var created struct {
		ID int64 `json:"id"`
	}
	if err := gh.Do(ctx, "POST", fmt.Sprintf("/repos/%s/check-runs", j.Repo), body, &created); err != nil {
		return 0, err
	}
	return created.ID, s.Store.SetCheckRun(ctx, j.ID, created.ID)
}

// statusOf maps a job to a GitHub commit status state and description.
func statusOf(j *Job) (string, string) {
	took := func() string { return roundDuration(j.FinishedAt.Sub(j.StartedAt)) }
	switch j.Status {
	case Waiting:
		return "pending", "Waiting for the jobs it needs"
	case Queued:
		return "pending", "Queued"
	case Running:
		return "pending", "Running on " + j.Worker
	case Success:
		return "success", "Passed in " + took()
	case Failure:
		if j.StartedAt.IsZero() {
			return "failure", "Failed before it started"
		}
		return "failure", "Failed after " + took()
	case Cancelled:
		return "error", "Cancelled"
	case Skipped:
		return "success", "Skipped"
	default:
		return "error", "The worker stopped responding"
	}
}

func roundDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}
