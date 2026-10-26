package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nektos/act/pkg/gh"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/broisnischal/selfaction/internal/agent"
)

func agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run this machine as GitHub Actions self-hosted runners",
		Long: `The agent registers single-use (just-in-time) runners with GitHub and runs
the official actions/runner, so jobs with runs-on: [self-hosted, <label>] land
here with full GitHub compatibility. Each slot runs one job at a time and is
cleaned after every job.

Authentication: --token, --token-file, $SELFACTION_GITHUB_TOKEN, $GITHUB_TOKEN,
then ` + "`gh auth token`" + `. Repository runners need admin access to the repo
(classic PAT scope "repo", or fine-grained "Administration: write");
organization runners need "admin:org" / "Self-hosted runners: write".`,
	}
	cmd.AddCommand(agentStartCmd())
	return cmd
}

type agentFlags struct {
	cfg       agent.Config
	repo, org string
	token     string
	tokenFile string
	api       string
}

func (a *agentFlags) bind(f *pflag.FlagSet) {
	host, _ := os.Hostname()
	f.StringVar(&a.repo, "repo", "", "serve one repository: owner/repo")
	f.StringVar(&a.org, "org", "", "serve every repository in an organization")
	f.StringSliceVarP(&a.cfg.Labels, "labels", "l", nil, "extra runner labels (self-hosted, OS and arch are always added)")
	f.IntVarP(&a.cfg.Slots, "slots", "n", 1, "concurrent jobs")
	f.StringVar(&a.cfg.Mode, "mode", "host", "host: jobs run directly on this machine; docker: each job in a fresh runner container")
	f.StringVar(&a.cfg.Name, "name", runnerName(host), "runner name prefix")
	f.Int64Var(&a.cfg.GroupID, "runner-group", 1, "runner group id (1 = Default)")
	f.StringVar(&a.cfg.RunnerVersion, "runner-version", "", "actions/runner version for host mode (default latest)")
	f.StringVar(&a.cfg.Image, "image", "ghcr.io/actions/actions-runner:latest", "runner image for docker mode")
	f.BoolVar(&a.cfg.DockerSocket, "docker-socket", false, "docker mode: mount the host Docker socket so jobs can use container:, services: and docker build (gives jobs root on the Docker host)")
	f.StringVar(&a.token, "token", "", "GitHub token")
	f.StringVar(&a.tokenFile, "token-file", "", "read the GitHub token from this file")
	f.StringVar(&a.api, "github-api", "https://api.github.com", "GitHub API base; for GitHub Enterprise Server use https://<host>/api/v3")
}

// runnerName makes a hostname safe for a runner name ("Nees-MacBook.local" → "nees-macbook").
func runnerName(host string) string {
	host, _, _ = strings.Cut(strings.ToLower(host), ".")
	name := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, host)
	if name == "" {
		return "selfaction"
	}
	return name
}

func (a *agentFlags) target() (string, error) {
	switch {
	case a.repo != "" && a.org != "":
		return "", errors.New("pass --repo or --org, not both")
	case a.repo != "":
		if strings.Count(a.repo, "/") != 1 {
			return "", fmt.Errorf("--repo must look like owner/repo, got %q", a.repo)
		}
		return a.repo, nil
	case a.org != "":
		return a.org, nil
	}
	return "", errors.New("pass --repo owner/repo or --org name")
}

// resolveToken returns the token and whether it came from the gh CLI (which a
// service can call itself, so it never needs to be written to disk).
func (a *agentFlags) resolveToken(ctx context.Context) (string, bool, error) {
	if a.token != "" {
		return a.token, false, nil
	}
	if a.tokenFile != "" {
		b, err := os.ReadFile(a.tokenFile)
		return strings.TrimSpace(string(b)), false, err
	}
	for _, env := range []string{"SELFACTION_GITHUB_TOKEN", "GITHUB_TOKEN"} {
		if t := os.Getenv(env); t != "" {
			return t, false, nil
		}
	}
	if t, err := gh.GetToken(ctx, ""); err == nil && t != "" {
		return t, true, nil
	}
	return "", false, errors.New("no GitHub token: pass --token/--token-file, set GITHUB_TOKEN, or run `gh auth login`")
}

func agentStartCmd() *cobra.Command {
	var a agentFlags
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Register runners and run jobs until stopped",
		Example: `  selfaction agent start --repo octo/app --labels my-mac
  selfaction agent start --org octo --slots 4 --mode docker --docker-socket`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target, err := a.target()
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			token, _, err := a.resolveToken(ctx)
			if err != nil {
				return err
			}
			a.cfg.Target = target
			a.cfg.Dir = filepath.Join(homeDir, "agent")
			ag := &agent.Agent{
				Config: a.cfg,
				GitHub: &agent.GitHub{API: strings.TrimSuffix(a.api, "/"), Token: token},
				Log:    log.StandardLogger(),
			}
			return ag.Run(ctx)
		},
	}
	a.bind(cmd.Flags())
	return cmd
}
