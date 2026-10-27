package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/broisnees/selfaction/internal/run"
	"github.com/broisnees/selfaction/internal/worker"
)

func workerCmd() *cobra.Command {
	var w worker.Worker
	var token string
	var extra []string
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run jobs from a selfaction server with the act engine",
		Long: `The worker claims jobs from a selfaction server, fetches the commit, runs the
workflow with the act engine (ubuntu-* jobs in Docker, self-hosted jobs on this
machine) and streams the log back. Each slot runs one job at a time.

It serves the runs-on labels of the built-in platforms (ubuntu-latest,
ubuntu-24.04, ubuntu-22.04, ubuntu-20.04, self-hosted, and macos-* on a Mac),
every label given with -P, and --labels.`,
		Example: `  selfaction worker --server https://ci.example.com --token "$SELFACTION_WORKER_TOKEN"
  selfaction worker --server http://ci.lan:8080 --slots 4 -P ubuntu-latest=catthehacker/ubuntu:full-latest`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			labels := slices.Collect(maps.Keys(run.DefaultPlatforms()))
			for _, p := range w.Platforms {
				label, _, ok := strings.Cut(p, "=")
				if !ok {
					return fmt.Errorf("-P %q: want label=image", p)
				}
				labels = append(labels, label)
			}
			labels = append(labels, extra...)
			for i, l := range labels {
				labels[i] = strings.ToLower(l)
			}
			slices.Sort(labels)
			w.Labels = slices.Compact(labels)

			var err error
			if w.Token, err = workerToken(token); err != nil {
				return err
			}
			w.Dir = filepath.Join(homeDir, "worker")
			w.Log = log.StandardLogger()
			ctx, cancel := signalContext()
			defer cancel()
			return w.Run(ctx)
		},
	}
	host, _ := os.Hostname()
	f := cmd.Flags()
	f.StringVar(&w.Server, "server", "", "server base URL, e.g. https://ci.example.com (required)")
	f.StringVar(&token, "token", os.Getenv("SELFACTION_WORKER_TOKEN"), "the server's worker token (env SELFACTION_WORKER_TOKEN; default: the one in <home>/server/worker-token)")
	f.StringVar(&w.Name, "name", runnerName(host), "worker name; slot n reports as <name>-<n>")
	f.IntVarP(&w.Slots, "slots", "n", 1, "concurrent jobs")
	f.StringSliceVarP(&extra, "labels", "l", nil, "extra runs-on labels to serve")
	f.StringArrayVarP(&w.Platforms, "platform", "P", nil, "runs-on label=image (image -self-hosted runs on this machine); also serves the label")
	f.StringVar(&w.Arch, "container-architecture", "", "job container platform, e.g. linux/amd64")
	f.DurationVar(&w.Timeout, "job-timeout", 6*time.Hour, "cancel a job that runs longer than this")
	_ = cmd.MarkFlagRequired("server")
	return cmd
}
