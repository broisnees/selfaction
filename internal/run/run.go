// Package run executes a repository's GitHub Actions workflows locally with
// nektos/act, backed by selfaction's persistent cache.
package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/nektos/act/pkg/artifacts"
	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/common/git"
	"github.com/nektos/act/pkg/container"
	"github.com/nektos/act/pkg/gh"
	"github.com/nektos/act/pkg/model"
	"github.com/nektos/act/pkg/runner"
	log "github.com/sirupsen/logrus"

	"github.com/broisnischal/selfaction/internal/cache"
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
	CacheDir    string // local cache store, used when CacheURL is empty
	CacheURL    string // shared cache server base: http://host:port/<token>
	NoCache     bool
	ArtifactDir string
	CachePolicy cache.Policy
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

	// Job containers reach the host through its LAN address, as act does.
	ip := common.GetOutboundIP()
	if ip == nil {
		return errors.New("cannot determine this machine's outbound IP for the cache and artifact servers")
	}

	if !o.NoCache {
		scope := cache.ScopeOf(repoName(ctx, dir))
		base := o.CacheURL
		if base == "" {
			stop, url, err := startCache(ctx, o.CacheDir, ip, o.CachePolicy)
			if err != nil {
				return fmt.Errorf("start cache server: %w", err)
			}
			defer stop()
			base = url
		}
		env["ACTIONS_CACHE_URL"] = strings.TrimSuffix(base, "/") + "/" + scope + "/"
		log.Infof("cache: scope %s", scope)
	}

	artifactPort := ""
	if o.ArtifactDir != "" {
		port, err := freePort(ip)
		if err != nil {
			return err
		}
		artifactPort = strconv.Itoa(port)
		stop := artifacts.Serve(ctx, o.ArtifactDir, ip.String(), artifactPort)
		defer stop()
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
		ArtifactServerPath:    o.ArtifactDir,
		ArtifactServerAddr:    ip.String(),
		ArtifactServerPort:    artifactPort,
		ConcurrentJobs:        o.Concurrency,
	})
	if err != nil {
		return err
	}
	return r.NewPlanExecutor(plan)(ctx)
}

// startCache serves a local store on an ephemeral port for the length of the run.
func startCache(ctx context.Context, dir string, ip net.IP, p cache.Policy) (func(), string, error) {
	store, err := cache.Open(dir)
	if err != nil {
		return nil, "", err
	}
	if n, _, err := store.GC(ctx, p); err == nil && n > 0 {
		log.Infof("cache: evicted %d old entries", n)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		store.Close()
		return nil, "", err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	srv := &http.Server{Handler: (&cache.Server{Store: store, Token: token, Log: log.StandardLogger()}).Handler()}
	go func() { _ = srv.Serve(ln) }()
	stop := func() {
		_ = srv.Close()
		_ = store.Close()
	}
	return stop, fmt.Sprintf("http://%s/%s", ln.Addr(), token), nil
}

func freePort(ip net.IP) (int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// repoName is owner/repo from the origin remote, or the directory name.
func repoName(ctx context.Context, dir string) string {
	if repo, err := git.FindGithubRepo(ctx, dir, "github.com", "origin"); err == nil && repo != "" {
		return repo
	}
	return filepath.Base(dir)
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
