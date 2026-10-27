package main

import (
	"path/filepath"
	"time"

	"github.com/nektos/act/pkg/common"
	"github.com/spf13/cobra"

	"github.com/broisnischal/selfaction/internal/cache"
	"github.com/broisnischal/selfaction/internal/run"
)

func runCmd() *cobra.Command {
	var o run.Options
	var artifacts bool
	cmd := &cobra.Command{
		Use:   "run [event]",
		Short: "Run this repository's workflows locally in Docker (act engine, persistent cache)",
		Example: `  selfaction run                      # the workflows' only event, else push
  selfaction run pull_request -j test
  selfaction run --list
  selfaction run -s NPM_TOKEN -P ubuntu-latest=catthehacker/ubuntu:full-latest
  selfaction run --cache-url http://cache.lan:7979/<token>   # share a team cache`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				o.Event = args[0]
			}
			if o.CacheDir == "" {
				o.CacheDir = filepath.Join(homeDir, "cache")
			}
			if artifacts {
				o.ArtifactDir = filepath.Join(homeDir, "artifacts")
			}
			o.CachePolicy = cache.Policy{TTL: 7 * 24 * time.Hour, MaxScopeBytes: 10 << 30}
			// First Ctrl-C cancels jobs gracefully (post steps run), second one aborts.
			ctx, cancel := common.CreateGracefulJobCancellationContext()
			defer cancel()
			return run.Run(ctx, o)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.Dir, "directory", "C", ".", "repository root")
	f.StringVarP(&o.Workflows, "workflows", "W", ".github/workflows", "workflow file or directory")
	f.StringVarP(&o.Job, "job", "j", "", "run only this job id")
	f.StringVarP(&o.EventPath, "eventpath", "e", "", "event payload JSON file")
	f.BoolVarP(&o.List, "list", "l", false, "list the jobs that would run")
	f.StringArrayVarP(&o.Platforms, "platform", "P", nil, "runs-on label=image (image -self-hosted runs on this machine)")
	f.StringArrayVarP(&o.Secrets, "secret", "s", nil, "secret KEY=VALUE, or KEY to read from the environment (GITHUB_TOKEN defaults to the gh CLI login)")
	f.StringArrayVar(&o.Vars, "var", nil, "configuration variable KEY=VALUE")
	f.StringArrayVar(&o.Env, "env", nil, "environment variable KEY=VALUE")
	f.StringArrayVar(&o.Matrix, "matrix", nil, "only run matrix combinations with key:value")
	f.StringVar(&o.Arch, "container-architecture", "", "job container platform, e.g. linux/amd64")
	f.IntVar(&o.Concurrency, "concurrency", 0, "max parallel jobs (default: CPU count)")
	f.BoolVar(&o.Pull, "pull", false, "always pull job images")
	f.StringVar(&o.CacheDir, "cache-dir", "", "local cache store (default <home>/cache)")
	f.StringVar(&o.CacheURL, "cache-url", "", "shared cache server from selfaction cache serve, instead of the local store: http://host:port/<token>")
	f.BoolVar(&o.NoCache, "no-cache", false, "disable actions/cache support")
	f.BoolVar(&artifacts, "artifacts", true, "serve actions/upload-artifact (stored in <home>/artifacts)")
	return cmd
}
