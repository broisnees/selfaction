package server

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"time"
)

// Claim is what a worker receives with a job.
type Claim struct {
	Job      *Job            `json:"job"`
	Event    json.RawMessage `json:"event"`     // the webhook payload, for github.event
	Token    string          `json:"token"`     // GitHub token for cloning and as the job's GITHUB_TOKEN
	CacheURL string          `json:"cache_url"` // actions/cache server, if the server offers one
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
			s.Log.Infof("job %s (%s %s) claimed by %s", j.ID, j.Repo, j.Name, j.Worker)
			s.report(j)
			writeJSON(w, Claim{Job: j, Event: j.Payload, Token: s.GitHub.Token, CacheURL: s.CacheURL})
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
		Worker string `json:"worker"`
		Result string `json:"result"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	j, err := s.Store.Finish(r.Context(), r.PathValue("id"), req.Worker, req.Result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.Log.Infof("job %s (%s %s): %s", j.ID, j.Repo, j.Name, j.Status)
	s.report(j)
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
	}
	writeJSON(w, j)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.List(r.Context(), 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, jobs)
}

func (s *Server) rawLog(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.Get(r.Context(), r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	b, _ := s.Store.Log(r.PathValue("id"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

var jobPage = template.Must(template.New("job").Funcs(template.FuncMap{
	"took": func(j *Job) string {
		switch {
		case j.StartedAt.IsZero():
			return ""
		case j.FinishedAt.IsZero():
			return roundDuration(time.Since(j.StartedAt)) + " so far"
		default:
			return roundDuration(j.FinishedAt.Sub(j.StartedAt))
		}
	},
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
{{if not .Job.Done}}<meta http-equiv="refresh" content="3">{{end}}
<title>{{.Job.Name}} {{.Job.Status}} · {{.Job.Repo}}</title>
<style>
:root{color-scheme:light dark;--ink:#0e1a33;--soft:#3b4a63;--rule:#cbd3dd;--bg:#fff;--log:#eef1f4}
@media (prefers-color-scheme:dark){:root{--ink:#e6ebf2;--soft:#9fb0c8;--rule:#26344f;--bg:#0d1628;--log:#131f36}}
body{margin:0;padding:24px 16px;background:var(--bg);color:var(--ink);font:16px/1.5 system-ui,sans-serif}
main{max-width:72rem;margin:0 auto}h1{margin:0;font-size:1.6rem}p{margin:.25rem 0;color:var(--soft)}a{color:inherit}
.s{font-weight:600}.success{color:#1a7f4b}.failure,.error{color:#c23b2e}.cancelled{color:var(--soft)}.queued,.running{color:#b45309}
pre{margin-top:1.25rem;padding:1rem;background:var(--log);border:1px solid var(--rule);border-radius:8px;overflow-x:auto;font:13px/1.55 ui-monospace,monospace;white-space:pre-wrap;word-break:break-word}
</style></head><body><main>
<h1>{{.Job.Name}} <span class="s {{.Job.Status}}">{{.Job.Status}}</span></h1>
<p>{{.Job.Repo}}, {{.Job.Event}} on {{.Job.Ref}}, commit <a href="https://github.com/{{.Job.Repo}}/commit/{{.Job.SHA}}">{{printf "%.7s" .Job.SHA}}</a>, workflow {{.Job.Workflow}}</p>
<p>{{if .Job.Worker}}Worker {{.Job.Worker}}. {{end}}{{took .Job}}</p>
<pre>{{if .Log}}{{.Log}}{{else}}Waiting for a worker…{{end}}</pre>
</main></body></html>`))

// page shows a job and its log. Job ids are random, so the page is safe to
// link from public commit statuses.
func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	j, err := s.Store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, _ := s.Store.Log(j.ID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = jobPage.Execute(w, map[string]any{"Job": j, "Log": string(b)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
