package server

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/workflowpattern"
)

// trigger is one webhook event, in the terms workflow `on:` filters use.
type trigger struct {
	event  string   // push or pull_request
	action string   // pull_request activity: opened, synchronize, …
	ref    string   // push: the pushed ref; pull_request: the base branch's ref
	files  []string // changed paths; nil when unknown, which skips path filters
}

// Default pull_request activity types, as on GitHub.
var defaultPRTypes = []string{"opened", "synchronize", "reopened"}

// matches applies GitHub's trigger rules for t to wf.
func (t trigger) matches(wf *model.Workflow) (bool, error) {
	if !slices.Contains(wf.On(), t.event) {
		return false, nil
	}
	cfg, _ := wf.OnEvent(t.event).(map[string]any) // nil: `on: push` without filters

	if t.event == "pull_request" {
		types := strs(cfg["types"])
		if len(types) == 0 {
			types = defaultPRTypes
		}
		if !slices.Contains(types, t.action) {
			return false, nil
		}
	}

	name, isTag := strings.CutPrefix(t.ref, "refs/tags/")
	if !isTag {
		name = strings.TrimPrefix(t.ref, "refs/heads/")
	}
	hasBranches := cfg["branches"] != nil || cfg["branches-ignore"] != nil
	hasTags := t.event == "push" && (cfg["tags"] != nil || cfg["tags-ignore"] != nil)
	var ok bool
	var err error
	switch {
	// A workflow that filters only one kind of ref ignores events for the other.
	case isTag && hasBranches && !hasTags, !isTag && hasTags && !hasBranches:
		return false, nil
	case isTag:
		ok, err = allowed(cfg["tags"], cfg["tags-ignore"], []string{name})
	default:
		ok, err = allowed(cfg["branches"], cfg["branches-ignore"], []string{name})
	}
	if !ok || err != nil {
		return false, err
	}

	// Path filters are not evaluated for tag pushes, nor when the changes are unknown.
	if isTag || t.files == nil {
		return true, nil
	}
	return allowed(cfg["paths"], cfg["paths-ignore"], t.files)
}

// allowed applies an include and an ignore pattern list to inputs: some input
// must match include (if given), and not every input may match ignore.
func allowed(include, ignore any, inputs []string) (bool, error) {
	quiet := &workflowpattern.EmptyTraceWriter{}
	if p := strs(include); len(p) > 0 {
		re, err := workflowpattern.CompilePatterns(p...)
		if err != nil {
			return false, fmt.Errorf("invalid filter %q: %w", p, err)
		}
		if workflowpattern.Skip(re, inputs, quiet) {
			return false, nil
		}
	}
	if p := strs(ignore); len(p) > 0 {
		re, err := workflowpattern.CompilePatterns(p...)
		if err != nil {
			return false, fmt.Errorf("invalid filter %q: %w", p, err)
		}
		if workflowpattern.Filter(re, inputs, quiet) {
			return false, nil
		}
	}
	return true, nil
}

// strs reads a YAML value that may be a string or a list of strings.
func strs(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// requiredLabels is the union of the workflow's literal runs-on labels. Labels
// computed by expressions cannot be known before the run and are left out.
func requiredLabels(wf *model.Workflow) []string {
	var out []string
	for _, job := range wf.Jobs {
		for _, l := range job.RunsOn() {
			l = strings.ToLower(strings.TrimSpace(l))
			if l != "" && !strings.Contains(l, "${{") && !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	slices.Sort(out)
	return out
}

// workflowName is the workflow's name, or its file name without extension.
func workflowName(wf *model.Workflow, file string) string {
	if wf.Name != "" {
		return wf.Name
	}
	return strings.TrimSuffix(path.Base(file), path.Ext(file))
}
