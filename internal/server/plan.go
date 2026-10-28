package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"
	"gopkg.in/yaml.v3"
)

// A workflow run becomes one job row per workflow job and matrix combination.
// Rows start "waiting"; progress releases a row to "queued" once every job it
// needs has finished and its `if` holds, skips it otherwise, and expands
// matrices that depend on needs once those are known. A worker then runs that
// one job with the act engine, given the results and outputs of its needs.

// jobOrder lists a workflow's job ids in the order the file declares them.
func jobOrder(src []byte, wf *model.Workflow) []string {
	var doc yaml.Node
	var out []string
	if yaml.Unmarshal(src, &doc) == nil && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		root := doc.Content[0].Content
		for i := 0; i+1 < len(root); i += 2 {
			if root[i].Value == "jobs" && root[i+1].Kind == yaml.MappingNode {
				for k := 0; k+1 < len(root[i+1].Content); k += 2 {
					out = append(out, root[i+1].Content[k].Value)
				}
			}
		}
	}
	// Fall back to sorted ids for anything the walk missed.
	var rest []string
	for id := range wf.Jobs {
		if !slices.Contains(out, id) {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// validateNeeds rejects needs on unknown jobs and dependency cycles, which
// GitHub refuses to run.
func validateNeeds(wf *model.Workflow) error {
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(id string, path []string) error
	visit = func(id string, path []string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("jobs depend on each other in a cycle: %s", strings.Join(append(path, id), " -> "))
		case 2:
			return nil
		}
		state[id] = 1
		for _, n := range wf.Jobs[id].Needs() {
			if wf.Jobs[n] == nil {
				return fmt.Errorf("job %q needs %q, which does not exist", id, n)
			}
			if err := visit(n, append(path, id)); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(wf.Jobs))
	for id := range wf.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if wf.Jobs[id] == nil {
			return fmt.Errorf("job %q is empty", id)
		}
		if err := visit(id, nil); err != nil {
			return err
		}
	}
	return nil
}

// githubContext is the github context of a run, as far as the server knows it.
func githubContext(r *Run) *model.GithubContext {
	var event map[string]any
	_ = json.Unmarshal(r.Payload, &event)
	owner, _, _ := strings.Cut(r.Repo, "/")
	ghc := &model.GithubContext{
		Event:           event,
		EventName:       r.Event,
		Sha:             r.SHA,
		Ref:             r.Ref,
		Repository:      r.Repo,
		RepositoryOwner: owner,
		Workflow:        r.Name,
		RunID:           strconv.FormatInt(r.Number, 10),
		RunNumber:       strconv.FormatInt(r.RunNumber, 10),
		RunAttempt:      "1",
		RetentionDays:   "90",
		ServerURL:       "https://github.com",
		APIURL:          "https://api.github.com",
		GraphQLURL:      "https://api.github.com/graphql",
	}
	if sender, ok := event["sender"].(map[string]any); ok {
		ghc.Actor, _ = sender["login"].(string)
	}
	if name, ok := strings.CutPrefix(r.Ref, "refs/tags/"); ok {
		ghc.RefType, ghc.RefName = "tag", name
	} else {
		ghc.RefType, ghc.RefName = "branch", strings.TrimPrefix(r.Ref, "refs/heads/")
	}
	if pr, ok := event["pull_request"].(map[string]any); ok && r.Event == "pull_request" {
		num, _ := pr["number"].(float64)
		ghc.Ref = fmt.Sprintf("refs/pull/%d/merge", int(num))
		ghc.RefName = fmt.Sprintf("%d/merge", int(num))
		if head, ok := pr["head"].(map[string]any); ok {
			ghc.HeadRef, _ = head["ref"].(string)
		}
		if base, ok := pr["base"].(map[string]any); ok {
			ghc.BaseRef, _ = base["ref"].(string)
		}
	}
	return ghc
}

// aggregate is the needs.<job>.result of a job's rows, GitHub style, and
// whether they have all finished.
func aggregate(rows []*Job) (result string, outputs map[string]string, done bool) {
	if len(rows) == 0 {
		return "", nil, false
	}
	failed, cancelled, skipped := false, false, 0
	outputs = map[string]string{}
	for _, r := range rows {
		switch r.Status {
		case Failure, Errored:
			failed = true
		case Cancelled:
			cancelled = true
		case Skipped:
			skipped++
		case Success:
		default:
			return "", nil, false
		}
		for k, v := range r.Outputs {
			if v != "" || outputs[k] == "" {
				outputs[k] = v // a matrix job's outputs come from whichever combination set them
			}
		}
	}
	switch {
	case failed:
		return "failure", outputs, true
	case cancelled:
		return "cancelled", outputs, true
	case skipped == len(rows):
		return "skipped", outputs, true
	}
	return "success", outputs, true
}

// Need is the result and outputs of a job another job needs.
type Need struct {
	Result  string            `json:"result"`
	Outputs map[string]string `json:"outputs"`
}

// transitiveNeeds lists every job key needs, directly or not.
func transitiveNeeds(wf *model.Workflow, key string) []string {
	var out []string
	var walk func(string)
	walk = func(k string) {
		j := wf.GetJob(k)
		if j == nil {
			return
		}
		for _, n := range j.Needs() {
			if !slices.Contains(out, n) {
				out = append(out, n)
				walk(n)
			}
		}
	}
	walk(key)
	return out
}

// needsFor is what a worker needs to know about the jobs key depends on.
func needsFor(wf *model.Workflow, key string, rows []*Job) map[string]Need {
	byKey := map[string][]*Job{}
	for _, r := range rows {
		byKey[r.Key] = append(byKey[r.Key], r)
	}
	out := map[string]Need{}
	for _, n := range transitiveNeeds(wf, key) {
		result, outputs, _ := aggregate(byKey[n])
		out[n] = Need{Result: result, Outputs: outputs}
	}
	return out
}

// staticMatrix reports whether a job's matrix can be expanded without
// evaluating expressions.
func staticMatrix(job *model.Job) bool {
	if job.Strategy == nil {
		return true
	}
	b, _ := yaml.Marshal(&job.Strategy.RawMatrix)
	return !bytes.Contains(b, []byte("${{"))
}

// combinations expands a job's matrix; a job without one has a single empty combination.
func combinations(job *model.Job, ev *evaluator) ([]map[string]any, error) {
	if job.Strategy == nil || job.Strategy.RawMatrix.Kind == 0 {
		return []map[string]any{nil}, nil
	}
	c := *job
	s := *job.Strategy
	s.RawMatrix = cloneNode(job.Strategy.RawMatrix)
	c.Strategy = &s
	if ev != nil {
		if err := ev.node(&c.Strategy.RawMatrix); err != nil {
			return nil, fmt.Errorf("strategy.matrix: %w", err)
		}
	}
	if c.Strategy.RawMatrix.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("strategy.matrix must be a mapping")
	}
	vectors := c.Matrix()
	if vectors == nil {
		return nil, fmt.Errorf("strategy.matrix: every key except include and exclude must be a list")
	}
	for k, v := range vectors {
		if k != "include" && k != "exclude" && len(v) == 0 {
			return nil, fmt.Errorf("matrix vector %q has no values", k) // GitHub fails the job too
		}
	}
	m, err := c.GetMatrixes()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(m))
	for _, combo := range m {
		if len(combo) == 0 {
			combo = nil
		}
		out = append(out, combo)
	}
	return out, nil
}

// cloneNode deep-copies a YAML value, so evaluating it leaves the workflow alone.
func cloneNode(n yaml.Node) yaml.Node {
	b, err := yaml.Marshal(&n)
	var doc yaml.Node
	if err != nil || yaml.Unmarshal(b, &doc) != nil || len(doc.Content) == 0 {
		return n
	}
	return *doc.Content[0]
}

// matrixKeys orders a combination's keys as the matrix declares them.
func matrixKeys(job *model.Job, combo map[string]any) []string {
	var keys []string
	if job.Strategy != nil && job.Strategy.RawMatrix.Kind == yaml.MappingNode {
		c := job.Strategy.RawMatrix.Content
		for i := 0; i+1 < len(c); i += 2 {
			if k := c[i].Value; k != "include" && k != "exclude" {
				if _, ok := combo[k]; ok {
					keys = append(keys, k)
				}
			}
		}
	}
	var rest []string
	for k := range combo {
		if !slices.Contains(keys, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// displayName names a job like GitHub does: its name (with matrix
// expressions evaluated), or its id followed by the matrix values.
func displayName(job *model.Job, key string, combo map[string]any, ev *evaluator) string {
	name := job.Name
	if name == "" {
		name = key
	}
	if strings.Contains(name, "${{") {
		e := *ev
		e.matrix = combo
		if s, err := e.interpolate(name); err == nil {
			return strings.TrimSpace(s)
		}
		return name
	}
	if len(combo) == 0 {
		return name
	}
	var vals []string
	for _, k := range matrixKeys(job, combo) {
		switch v := combo[k].(type) {
		case string:
			vals = append(vals, v)
		case map[string]any, []any:
			b, _ := json.Marshal(v)
			vals = append(vals, string(b))
		default:
			vals = append(vals, fmt.Sprint(v))
		}
	}
	return name + " (" + strings.Join(vals, ", ") + ")"
}

// runsOn evaluates a job's runs-on labels for one combination, in order.
func runsOn(job *model.Job, ev *evaluator, combo map[string]any) ([]string, error) {
	e := *ev
	e.matrix = combo
	out := []string{}
	add := func(l string) {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	for _, raw := range job.RunsOn() {
		v, err := e.value(raw)
		if err != nil {
			return nil, fmt.Errorf("runs-on: %w", err)
		}
		switch v := v.(type) {
		case string:
			add(v)
		case []any:
			for _, x := range v {
				add(fmt.Sprint(x))
			}
		case nil:
		default:
			add(fmt.Sprint(v))
		}
	}
	return out, nil
}

// strategyOptions reads fail-fast (default on) and max-parallel (default unlimited).
func strategyOptions(job *model.Job, ev *evaluator) (failFast bool, maxParallel int) {
	if job.Strategy == nil {
		return true, 0
	}
	failFast = true
	if s, err := ev.interpolate(job.Strategy.FailFastString); err == nil && s != "" {
		if b, err := strconv.ParseBool(s); err == nil {
			failFast = b
		}
	}
	if s, err := ev.interpolate(job.Strategy.MaxParallelString); err == nil && s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			maxParallel = n
		}
	}
	return failFast, maxParallel
}

// change is what one pass over a run decided.
type change struct {
	changed  []*Job            // waiting rows that were released, skipped, failed or expanded
	added    []*Job            // new rows: a run's first jobs, or a matrix's other combinations
	cancel   []*Job            // queued or running rows to cancel (fail-fast)
	messages map[*Job][]string // why a row failed or was skipped, for its log
}

func (c *change) note(j *Job, format string, args ...any) {
	if c.messages == nil {
		c.messages = map[*Job][]string{}
	}
	c.messages[j] = append(c.messages[j], fmt.Sprintf(format, args...))
}

func (c *change) mark(j *Job) {
	if !slices.Contains(c.changed, j) && !slices.Contains(c.added, j) {
		c.changed = append(c.changed, j)
	}
}

// progress decides what changes in a run given its rows so far. With no rows
// it creates the run's first ones.
func progress(run *Run, rows []*Job) (*change, error) {
	wf, err := model.ReadWorkflow(bytes.NewReader(run.Source), false)
	if err != nil {
		return nil, err
	}
	order := jobOrder(run.Source, wf)
	c := &change{}
	ghc := githubContext(run)
	byKey := map[string][]*Job{}
	for _, r := range rows {
		byKey[r.Key] = append(byKey[r.Key], r)
	}

	// New run: one waiting row per job and static matrix combination, or a
	// placeholder for a matrix that needs expressions evaluated.
	if len(rows) == 0 {
		for _, key := range order {
			job := wf.Jobs[key]
			if !staticMatrix(job) {
				j := &Job{Key: key, Name: displayName(job, key, nil, &evaluator{github: ghc, wf: wf, key: key}), Expand: true, Status: Waiting}
				c.added = append(c.added, j)
				byKey[key] = append(byKey[key], j)
				continue
			}
			combos, err := combinations(job, nil)
			if err != nil {
				j := &Job{Key: key, Name: key, Status: Failure}
				c.note(j, "Invalid matrix: %v", err)
				c.added = append(c.added, j)
				byKey[key] = append(byKey[key], j)
				continue
			}
			for _, combo := range combos {
				j := &Job{Key: key, Name: displayName(job, key, combo, &evaluator{github: ghc, wf: wf, key: key}), Matrix: combo, Status: Waiting}
				c.added = append(c.added, j)
				byKey[key] = append(byKey[key], j)
			}
		}
	}

	for {
		moved := false
		results := map[string]exprparser.Needs{}
		for key, rs := range byKey {
			if result, outputs, done := aggregate(rs); done {
				results[key] = exprparser.Needs{Result: result, Outputs: outputs}
				if j := wf.Jobs[key]; j != nil { // nil only if a row names a job the stored workflow lacks
					j.Result = result
				}
			}
		}
		for _, key := range order {
			job, rs := wf.Jobs[key], byKey[key]
			ev := &evaluator{github: ghc, wf: wf, key: key, needs: map[string]exprparser.Needs{}}
			failFast, maxParallel := strategyOptions(job, ev)

			// fail-fast: one failed combination cancels the others.
			if failFast && len(rs) > 1 && slices.ContainsFunc(rs, func(r *Job) bool { return r.Status == Failure || r.Status == Errored }) {
				for _, r := range rs {
					switch r.Status {
					case Waiting:
						r.Status = Cancelled
						c.note(r, "Cancelled because another combination of %s failed (strategy.fail-fast).", key)
						c.mark(r)
						moved = true
					case Queued, Running:
						if !slices.Contains(c.cancel, r) {
							c.cancel = append(c.cancel, r)
						}
					}
				}
			}

			waiting := slices.DeleteFunc(slices.Clone(rs), func(r *Job) bool { return r.Status != Waiting })
			if len(waiting) == 0 {
				continue
			}
			ready := true
			for _, n := range job.Needs() {
				r, ok := results[n]
				if !ok {
					ready = false
					break
				}
				ev.needs[n] = r
			}
			if !ready {
				continue
			}

			// The first release decides whether the job runs; later ones only
			// start more combinations as max-parallel allows. (A combination
			// cancelled while it waited says nothing about that decision.)
			started := slices.ContainsFunc(rs, func(r *Job) bool {
				return r.Status != Waiting && r.Status != Cancelled && r.Status != Skipped
			})
			if !started {
				ok, err := ev.enabled(job.If.Value)
				if err != nil || !ok {
					for _, r := range waiting {
						if err != nil {
							r.Status = Failure
							c.note(r, "Invalid if expression %q: %v", job.If.Value, err)
						} else {
							r.Status = Skipped
						}
						r.Expand = false // a matrix that never ran shows as the job itself
						c.mark(r)
					}
					moved = true
					continue
				}
			}

			if len(waiting) == 1 && waiting[0].Expand {
				p := waiting[0]
				combos, err := combinations(job, ev)
				if err != nil {
					p.Status, p.Expand = Failure, false
					c.note(p, "Invalid matrix: %v", err)
					c.mark(p)
					moved = true
					continue
				}
				p.Expand, p.Matrix, p.Name = false, combos[0], displayName(job, key, combos[0], ev)
				c.mark(p)
				for _, combo := range combos[1:] {
					j := &Job{Key: key, RunID: run.ID, Name: displayName(job, key, combo, ev), Matrix: combo, Status: Waiting}
					j.fromRun(run)
					c.added = append(c.added, j)
					byKey[key] = append(byKey[key], j)
					waiting = append(waiting, j)
				}
				rs = byKey[key]
			}

			active := 0
			for _, r := range rs {
				if r.Status == Queued || r.Status == Running {
					active++
				}
			}
			for _, r := range waiting {
				if maxParallel > 0 && active >= maxParallel {
					break
				}
				labels, err := runsOn(job, ev, r.Matrix)
				if err != nil {
					r.Status = Failure
					c.note(r, "%v", err)
					moved = true
				} else {
					r.Labels, r.Status = labels, Queued
					r.Name = displayName(job, key, r.Matrix, ev)
					active++
				}
				c.mark(r)
			}
		}
		if !moved {
			return c, nil
		}
	}
}
