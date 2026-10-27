package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nektos/act/pkg/gh"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/broisnees/selfaction/internal/agent"
	"github.com/broisnees/selfaction/internal/github"
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
	cmd.AddCommand(agentStartCmd(), agentInstallCmd(), agentUninstallCmd(), agentCleanCmd())
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
	f.BoolVar(&a.cfg.ShareHome, "share-home", false, "host mode: run jobs with your own HOME, sharing your caches, dotfiles and credentials with them (default: a per-slot HOME)")
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
				GitHub: &agent.GitHub{Client: github.Client{API: strings.TrimSuffix(a.api, "/"), Token: token}},
				Log:    log.StandardLogger(),
			}
			return ag.Run(ctx)
		},
	}
	a.bind(cmd.Flags())
	return cmd
}

func agentInstallCmd() *cobra.Command {
	var a agentFlags
	cmd := &cobra.Command{
		Use:     "install",
		Short:   "Install the agent as a login service (launchd on macOS, systemd --user on Linux)",
		Example: `  selfaction agent install --repo octo/app --labels my-mac --slots 2`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := a.target(); err != nil {
				return err
			}
			token, fromGh, err := a.resolveToken(cmd.Context())
			if err != nil {
				return err
			}
			// Forward every flag the user set; persist an explicit token to a 0600 file
			// instead of the service definition.
			args := []string{"--home=" + homeDir} // the service won't see $SELFACTION_HOME
			cmd.Flags().Visit(func(f *pflag.Flag) {
				if f.Name == "token" || f.Name == "token-file" || f.Name == "home" {
					return
				}
				v := f.Value.String()
				if sv, ok := f.Value.(pflag.SliceValue); ok {
					v = strings.Join(sv.GetSlice(), ",")
				}
				args = append(args, "--"+f.Name+"="+v)
			})
			if !fromGh {
				tokenFile := a.tokenFile
				if tokenFile == "" {
					tokenFile = filepath.Join(homeDir, "agent", "token")
					if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
						return err
					}
					if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
						return err
					}
				}
				args = append(args, "--token-file="+tokenFile)
			}
			logFile := filepath.Join(homeDir, "agent.log")
			path, err := agent.Install(args, logFile)
			if err != nil {
				return err
			}
			fmt.Printf("installed %s\nlogs: %s\n", path, logFile)
			return nil
		},
	}
	a.bind(cmd.Flags())
	return cmd
}

func agentUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the agent service",
		RunE:  func(*cobra.Command, []string) error { return agent.Uninstall() },
	}
}

func agentCleanCmd() *cobra.Command {
	var tools, runners, docker bool
	var dockerAge time.Duration
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Free disk used by the agent (run while it is stopped)",
		Long: `Wipes runner workspaces, diagnostics and leftover credentials. The agent
already does this after every job; use it after a crash or to reclaim more space.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := agent.Clean(filepath.Join(homeDir, "agent"), tools, runners); err != nil {
				return err
			}
			if docker {
				if runtime.GOOS == "windows" {
					return errors.New("--docker is not supported on windows")
				}
				out, err := agent.PruneDocker(cmd.Context(), dockerAge)
				fmt.Print(string(out))
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&tools, "tools", false, "also delete tool caches and per-slot home directories (re-downloaded on next use)")
	cmd.Flags().BoolVar(&runners, "runners", false, "delete the runner installs and downloads entirely (re-downloaded on next start)")
	cmd.Flags().BoolVar(&docker, "docker", false, "also prune Docker: stopped containers, unused networks, dangling images and build cache older than --docker-age. Affects the whole Docker host, not just CI")
	cmd.Flags().DurationVar(&dockerAge, "docker-age", 72*time.Hour, "only prune Docker objects older than this")
	return cmd
}
