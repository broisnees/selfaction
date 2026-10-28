package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/broisnees/selfaction/internal/run"
	"github.com/broisnees/selfaction/internal/worker"
)

type workerFlags struct {
	w         worker.Worker
	token     string
	tokenFile string
	extra     []string
	noDefault bool
}

func (wf *workerFlags) bind(f *pflag.FlagSet) {
	host, _ := os.Hostname()
	f.StringVar(&wf.w.Server, "server", "", "server base URL, e.g. https://ci.example.com (required)")
	f.StringVar(&wf.token, "token", os.Getenv("SELFACTION_WORKER_TOKEN"), "the server's worker token (env SELFACTION_WORKER_TOKEN; default: the one in <home>/server/worker-token)")
	f.StringVar(&wf.tokenFile, "token-file", "", "read the worker token from this file")
	f.StringVar(&wf.w.Name, "name", runnerName(host), "worker name; slot n reports as <name>-<n>")
	f.IntVarP(&wf.w.Slots, "slots", "n", 1, "concurrent jobs")
	f.StringSliceVarP(&wf.extra, "labels", "l", nil, "extra runs-on labels to serve")
	f.BoolVar(&wf.noDefault, "no-default-labels", false, "serve only the labels given with -P and --labels, not the built-in ubuntu-*, self-hosted, macos-* and windows-* ones")
	f.StringArrayVarP(&wf.w.Platforms, "platform", "P", nil, "runs-on label=image (image -self-hosted runs on this machine); also serves the label")
	f.StringVar(&wf.w.Arch, "container-architecture", "", "job container platform, e.g. linux/amd64")
	f.DurationVar(&wf.w.Timeout, "job-timeout", 6*time.Hour, "cancel a job that runs longer than this")
	f.BoolVar(&wf.w.ShareHome, "share-home", false, "run jobs on this machine with your own HOME, sharing your caches, dotfiles and credentials with them (default: a HOME per slot)")
}

// resolveToken finds the worker token: --token, --token-file, then the file a
// server on this machine keeps.
func (wf *workerFlags) resolveToken() (string, error) {
	if wf.token == "" && wf.tokenFile != "" {
		b, err := os.ReadFile(wf.tokenFile)
		return strings.TrimSpace(string(b)), err
	}
	return workerToken(wf.token)
}

// labels are the runs-on labels this worker serves.
func (wf *workerFlags) labels() ([]string, error) {
	var labels []string
	if !wf.noDefault {
		labels = slices.Collect(maps.Keys(run.DefaultPlatforms()))
	}
	for _, p := range wf.w.Platforms {
		label, _, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("-P %q: want label=image", p)
		}
		labels = append(labels, label)
	}
	labels = append(labels, wf.extra...)
	for i, l := range labels {
		labels[i] = strings.ToLower(l)
	}
	slices.Sort(labels)
	if labels = slices.Compact(labels); len(labels) == 0 {
		return nil, errors.New("no labels to serve: give -P or --labels with --no-default-labels")
	}
	return labels, nil
}

func workerCmd() *cobra.Command {
	var wf workerFlags
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run jobs from a selfaction server with the act engine",
		Long: `The worker claims jobs from a selfaction server, fetches the commit, runs the
job with the act engine (ubuntu-* jobs in Docker, self-hosted, macos-* and
windows-* jobs on this machine) and streams the log back. Each slot runs one
job at a time. Jobs that run on this machine get a HOME of their own per slot.

It serves the runs-on labels of the built-in platforms (ubuntu-latest,
ubuntu-24.04, ubuntu-22.04, ubuntu-20.04, self-hosted, ubuntu-*-arm on ARM64,
macos-* on a Mac and windows-* on Windows), every label given with -P, and
--labels. --no-default-labels serves only the last two.`,
		Example: `  selfaction worker --server https://ci.example.com --token "$SELFACTION_WORKER_TOKEN"
  selfaction worker --server http://ci.lan:8080 --slots 4 -P ubuntu-latest=catthehacker/ubuntu:full-latest
  selfaction worker --server https://ci.example.com --labels xcode     # a Mac that builds iOS apps
  selfaction worker install --server https://ci.example.com --token "$TOKEN"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if wf.w.Server == "" {
				return errors.New("--server is required")
			}
			var err error
			if wf.w.Labels, err = wf.labels(); err != nil {
				return err
			}
			if wf.w.Token, err = wf.resolveToken(); err != nil {
				return err
			}
			wf.w.Dir = filepath.Join(homeDir, "worker")
			wf.w.Log = log.StandardLogger()
			ctx, cancel := signalContext()
			defer cancel()
			return wf.w.Run(ctx)
		},
	}
	wf.bind(cmd.Flags())
	cmd.AddCommand(workerInstallCmd(), workerUninstallCmd())
	return cmd
}

func workerInstallCmd() *cobra.Command {
	var wf workerFlags
	cmd := &cobra.Command{
		Use:     "install",
		Short:   "Install the worker as a login service (launchd on macOS, systemd --user on Linux, a scheduled task on Windows)",
		Example: `  selfaction worker install --server https://ci.example.com --token "$TOKEN" --slots 2`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if wf.w.Server == "" {
				return errors.New("--server is required")
			}
			if _, err := wf.labels(); err != nil {
				return err
			}
			token, err := wf.resolveToken()
			if err != nil {
				return err
			}
			// Keep an explicit token in a 0600 file rather than in the service definition.
			args := append([]string{"worker"}, serviceArgs(cmd, "token", "token-file")...)
			tokenFile := wf.tokenFile
			if tokenFile == "" && wf.token != "" {
				tokenFile = filepath.Join(homeDir, "worker", "token")
				if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
					return err
				}
			}
			if tokenFile != "" {
				args = append(args, "--token-file="+tokenFile)
			}
			return installService("worker", args)
		},
	}
	wf.bind(cmd.Flags())
	return cmd
}

func workerUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the worker service",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return uninstallService("worker") },
	}
}
