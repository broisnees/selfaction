// Package run executes a repository's GitHub Actions workflows locally with
// nektos/act.
package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/nektos/act/pkg/container"
	"github.com/nektos/act/pkg/gh"
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
	log "github.com/sirupsen/logrus"
)

type Options struct {
	Dir         string   // repository root
	Workflows   string   // workflow file or directory, relative to Dir
	Event       string   // event to simulate; empty = the only event the workflows use, else push
	EventPath   string   // JSON payload for the event
	Job         string   // run only this job id
	List        bool     // print the plan instead of running it
	Platforms   []string // runs-on label=image overrides; image "-self-hosted" runs on this machine
	Secrets     []string // KEY=VALUE, or KEY to read it from the environment
	Vars        []string
	Env         []string
	Matrix      []string // key:value filters
	Arch        string   // container platform, e.g. linux/amd64
	Concurrency int
	Pull        bool
}

// DefaultPlatforms are the "medium" catthehacker images act recommends: small
// enough to pull quickly, complete enough for most marketplace actions.
// macOS labels run directly on a Mac host.
func DefaultPlatforms() map[string]string {
	p := map[string]string{
		"ubuntu-latest": "catthehacker/ubuntu:act-latest",
		"ubuntu-24.04":  "catthehacker/ubuntu:act-24.04",
		"ubuntu-22.04":  "catthehacker/ubuntu:act-22.04",
		"ubuntu-20.04":  "catthehacker/ubuntu:act-20.04",
		"self-hosted":   "-self-hosted",
	}
	if runtime.GOOS == "darwin" {
		for _, l := range []string{"macos-latest", "macos-15", "macos-14", "macos-13"} {
			p[l] = "-self-hosted"
		}
	}
	return p
}

func Run(ctx context.Context, o Options) error {
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return err
	}
	wfPath := o.Workflows
	if !filepath.IsAbs(wfPath) {
		wfPath = filepath.Join(dir, wfPath)
	}
	planner, err := model.NewWorkflowPlanner(wfPath, false, false)
	if err != nil {
		return err
	}
	event := o.Event
	if event == "" {
		event = "push"
		if events := planner.GetEvents(); len(events) == 1 {
			event = events[0]
		}
	}
	var plan *model.Plan
	if o.Job != "" {
		plan, err = planner.PlanJob(o.Job)
	} else {
		plan, err = planner.PlanEvent(event)
	}
	if plan == nil || len(plan.Stages) == 0 {
		if err == nil {
			err = fmt.Errorf("no jobs to run for event %q (use --list to see jobs)", event)
		}
		return err
	}
	if o.List {
		return printPlan(plan)
	}

	socket := ""
	if sh, err := container.GetSocketAndHost(""); err != nil {
		log.Warnf("no docker connection: %v (only -self-hosted platforms will work)", err)
	} else {
		os.Setenv("DOCKER_HOST", sh.Host)
		socket = sh.Socket
	}

	secrets := pairs(o.Secrets, true)
	if _, ok := secrets["GITHUB_TOKEN"]; !ok {
		secrets["GITHUB_TOKEN"], _ = gh.GetToken(ctx, dir)
	}
	env := pairs(o.Env, false)
	platforms := DefaultPlatforms()
	for k, v := range pairs(o.Platforms, false) {
		platforms[strings.ToLower(k)] = v
	}

	r, err := runner.New(&runner.Config{
		Workdir:               dir,
		EventName:             event,
		EventPath:             o.EventPath,
		ForcePull:             o.Pull,
		LogOutput:             true,
		Env:                   env,
		Secrets:               secrets,
		Vars:                  pairs(o.Vars, false),
		Token:                 secrets["GITHUB_TOKEN"],
		Platforms:             platforms,
		Matrix:                matrix(o.Matrix),
		ContainerArchitecture: o.Arch,
		ContainerDaemonSocket: socket,
		GitHubInstance:        "github.com",
		RemoteName:            "origin",
		UseGitIgnore:          true,
		AutoRemove:            true,
		ConcurrentJobs:        o.Concurrency,
	})
	if err != nil {
		return err
	}
	return r.NewPlanExecutor(plan)(ctx)
}

// pairs parses KEY=VALUE items. With fromEnv, a bare KEY takes its value from the environment.
func pairs(items []string, fromEnv bool) map[string]string {
	m := map[string]string{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok {
			if !fromEnv {
				continue
			}
			v = os.Getenv(k)
		}
		m[k] = v
	}
	return m
}

// matrix turns ["node:20", "os:ubuntu"] into act's matrix filter.
func matrix(items []string) map[string]map[string]bool {
	m := map[string]map[string]bool{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, ":")
		if !ok {
			continue
		}
		if m[k] == nil {
			m[k] = map[string]bool{}
		}
		m[k][v] = true
	}
	return m
}

func printPlan(plan *model.Plan) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STAGE\tJOB ID\tJOB NAME\tWORKFLOW\tFILE\tEVENTS")
	for i, stage := range plan.Stages {
		for _, r := range stage.Runs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", i, r.JobID, r.String(), r.Workflow.Name, r.Workflow.File, strings.Join(r.Workflow.On(), ","))
		}
	}
	return w.Flush()
}
