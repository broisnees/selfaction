package server

import (
	"html/template"
	"net/http"
	"time"

	"github.com/docker/go-units"
)

func (s *Server) rawLog(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.Get(r.Context(), r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	b, _ := s.Store.Log(r.PathValue("id"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

func took(j *Job) string {
	switch {
	case j.StartedAt.IsZero():
		return ""
	case j.FinishedAt.IsZero():
		return roundDuration(time.Since(j.StartedAt)) + " so far"
	default:
		return roundDuration(j.FinishedAt.Sub(j.StartedAt))
	}
}

const pageStyle = `<style>
:root{color-scheme:light dark;--ink:#0e1a33;--soft:#3b4a63;--rule:#cbd3dd;--bg:#fff;--log:#eef1f4}
@media (prefers-color-scheme:dark){:root{--ink:#e6ebf2;--soft:#9fb0c8;--rule:#26344f;--bg:#0d1628;--log:#131f36}}
body{margin:0;padding:24px 16px;background:var(--bg);color:var(--ink);font:16px/1.5 system-ui,sans-serif}
main{max-width:72rem;margin:0 auto}h1{margin:0;font-size:1.6rem}h2{font-size:1.1rem;margin:1.5rem 0 .5rem}p{margin:.25rem 0;color:var(--soft)}a{color:inherit}
.s{font-weight:600}.success{color:#1a7f4b}.failure,.error{color:#c23b2e}.cancelled,.skipped,.waiting{color:var(--soft)}.queued,.running{color:#b45309}
pre{margin-top:1.25rem;padding:1rem;background:var(--log);border:1px solid var(--rule);border-radius:8px;overflow-x:auto;font:13px/1.55 ui-monospace,monospace;white-space:pre-wrap;word-break:break-word}
table{border-collapse:collapse;width:100%}td{padding:.4rem .75rem .4rem 0;border-bottom:1px solid var(--rule);vertical-align:top}td:last-child{color:var(--soft);text-align:right}
</style>`

var funcs = template.FuncMap{"took": took, "size": func(n int64) string { return units.HumanSize(float64(n)) }}

var jobPage = template.Must(template.New("job").Funcs(funcs).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
{{if not .Job.Done}}<meta http-equiv="refresh" content="3">{{end}}
<title>{{.Job.Name}} {{.Job.Status}} · {{.Job.Repo}}</title>` + pageStyle + `</head><body><main>
<h1>{{.Job.Name}} <span class="s {{.Job.Status}}">{{.Job.Status}}</span></h1>
<p><a href="../runs/{{.Job.RunID}}">{{.Job.WorkflowName}}</a> on {{.Job.Repo}}, {{.Job.Event}} on {{.Job.Ref}}, commit <a href="https://github.com/{{.Job.Repo}}/commit/{{.Job.SHA}}">{{printf "%.7s" .Job.SHA}}</a></p>
<p>{{if .Job.Worker}}Worker {{.Job.Worker}}. {{end}}{{took .Job}}</p>
<pre>{{if .Log}}{{.Log}}{{else if eq .Job.Status "waiting"}}Waiting for the jobs this one needs…{{else if eq .Job.Status "skipped"}}Skipped.{{else}}Waiting for a worker…{{end}}</pre>
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

var runPage = template.Must(template.New("run").Funcs(funcs).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
{{if not .Done}}<meta http-equiv="refresh" content="5">{{end}}
<title>{{.Run.Name}} #{{.Run.RunNumber}} · {{.Run.Repo}}</title>` + pageStyle + `</head><body><main>
<h1>{{.Run.Name}} #{{.Run.RunNumber}} <span class="s {{.Status}}">{{.Status}}</span></h1>
<p>{{.Run.Repo}}, {{.Run.Event}} on {{.Run.Ref}}, commit <a href="https://github.com/{{.Run.Repo}}/commit/{{.Run.SHA}}">{{printf "%.7s" .Run.SHA}}</a>, workflow {{.Run.Workflow}}</p>
<h2>Jobs</h2>
<table>{{range .Jobs}}<tr><td><a href="../jobs/{{.ID}}">{{.Name}}</a></td><td class="s {{.Status}}">{{.Status}}</td><td>{{took .}}</td></tr>{{end}}</table>
{{if .Artifacts}}<h2>Artifacts</h2>
<table>{{range .Artifacts}}<tr><td><a href="{{$.Run.ID}}/artifacts/{{.Name}}">{{.Name}}</a></td><td></td><td>{{size .Size}}</td></tr>{{end}}</table>{{end}}
</main></body></html>`))

// runStatus sums up a run's jobs the way GitHub shows a workflow run.
func runStatus(jobs []*Job) (string, bool) {
	rows := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		if !j.Done() {
			if j.Status == Running {
				return Running, false
			}
			return Queued, false
		}
		rows = append(rows, j)
	}
	result, _, _ := aggregate(rows)
	return result, true
}

func (s *Server) runPage(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobs, err := s.Store.Jobs(r.Context(), run.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	arts, _ := s.Store.Artifacts(r.Context(), run.ID)
	status, done := runStatus(jobs)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = runPage.Execute(w, map[string]any{"Run": run, "Jobs": jobs, "Artifacts": arts, "Status": status, "Done": done})
}
