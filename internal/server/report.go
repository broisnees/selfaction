package server

import (
	"context"
	"fmt"
	"time"
)

// report posts j's state to GitHub as a commit status, in the background.
// Statuses show on the commit and on pull requests like checks do.
func (s *Server) report(j *Job) {
	state, desc := statusOf(j)
	body := map[string]string{
		"state":       state,
		"description": desc,
		"context":     fmt.Sprintf("selfaction/%s (%s)", j.Name, j.Event),
		"target_url":  s.jobURL(j.ID),
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.GitHub.Do(ctx, "POST", fmt.Sprintf("/repos/%s/statuses/%s", j.Repo, j.SHA), body, nil); err != nil {
			s.Log.Warnf("status for %s %s: %v", j.Repo, j.Name, err)
		}
	}()
}

// statusOf maps a job to a GitHub commit status state and description.
func statusOf(j *Job) (string, string) {
	took := func() string { return roundDuration(j.FinishedAt.Sub(j.StartedAt)) }
	switch j.Status {
	case Queued:
		return "pending", "Queued"
	case Running:
		return "pending", "Running on " + j.Worker
	case Success:
		return "success", "Passed in " + took()
	case Failure:
		return "failure", "Failed after " + took()
	case Cancelled:
		return "error", "Cancelled"
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
