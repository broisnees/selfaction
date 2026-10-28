package server

import (
	"strings"
	"testing"

	"github.com/nektos/act/pkg/model"
)

func workflow(t *testing.T, on string) *model.Workflow {
	t.Helper()
	y := "name: w\non:" + on + "\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: 'true'}]\n  b:\n    runs-on: [self-hosted, GPU]\n    steps: [{run: 'true'}]\n  c:\n    runs-on: ${{ matrix.os }}\n    steps: [{run: 'true'}]\n"
	wf, err := model.ReadWorkflow(strings.NewReader(y), false)
	if err != nil {
		t.Fatalf("parse %q: %v", on, err)
	}
	return wf
}

func TestTriggerMatchesGitHubRules(t *testing.T) {
	push := func(ref string, files ...string) trigger {
		if files == nil {
			return trigger{event: "push", ref: ref}
		}
		return trigger{event: "push", ref: ref, files: files}
	}
	pr := func(action, base string, files ...string) trigger {
		return trigger{event: "pull_request", action: action, ref: "refs/heads/" + base, files: files}
	}
	cases := []struct {
		on   string
		t    trigger
		want bool
	}{
		{" push", push("refs/heads/main"), true},
		{" [push, pull_request]", pr("opened", "main"), true},
		{" pull_request", push("refs/heads/main"), false},

		{"\n  push:\n    branches: [main, 'release/**']", push("refs/heads/release/1.2"), true},
		{"\n  push:\n    branches: [main]", push("refs/heads/feature"), false},
		{"\n  push:\n    branches-ignore: ['dependabot/**']", push("refs/heads/dependabot/go/x"), false},
		{"\n  push:\n    branches-ignore: ['dependabot/**']", push("refs/heads/main"), true},

		// Filtering only branches means tag pushes do not run, and the reverse.
		{"\n  push:\n    branches: [main]", push("refs/tags/v1.0.0"), false},
		{"\n  push:\n    tags: ['v*']", push("refs/tags/v1.0.0"), true},
		{"\n  push:\n    tags: ['v*']", push("refs/heads/main"), false},

		{"\n  push:\n    paths: ['src/**']", push("refs/heads/main", "src/a.go", "README.md"), true},
		{"\n  push:\n    paths: ['src/**']", push("refs/heads/main", "README.md"), false},
		{"\n  push:\n    paths-ignore: ['docs/**', '*.md']", push("refs/heads/main", "docs/x.md", "README.md"), false},
		{"\n  push:\n    paths-ignore: ['docs/**']", push("refs/heads/main", "docs/x.md", "main.go"), true},
		{"\n  push:\n    paths: ['src/**']", push("refs/heads/main"), true}, // changes unknown

		{"\n  pull_request:\n    branches: [main]", pr("synchronize", "main"), true},
		{"\n  pull_request:\n    branches: [main]", pr("opened", "develop"), false},
		{" pull_request", pr("closed", "main"), false}, // not a default type
		{"\n  pull_request:\n    types: [closed]", pr("closed", "main"), true},
		{"\n  pull_request:\n    paths: ['site/**']", pr("opened", "main", "site/a.astro"), true},
		{"\n  pull_request:\n    paths: ['site/**']", pr("opened", "main", "main.go"), false},
	}
	for _, c := range cases {
		got, err := c.t.matches(workflow(t, c.on))
		if err != nil || got != c.want {
			t.Errorf("on:%s with %s %s %v = %v (%v), want %v", strings.ReplaceAll(c.on, "\n", " "), c.t.event, c.t.ref, c.t.files, got, err, c.want)
		}
	}
}

func TestWorkflowName(t *testing.T) {
	wf := workflow(t, " push")
	if workflowName(wf, ".github/workflows/x.yml") != "w" {
		t.Error("name from workflow")
	}
	wf.Name = ""
	if workflowName(wf, ".github/workflows/x.yml") != "x" {
		t.Error("name from file")
	}
}
