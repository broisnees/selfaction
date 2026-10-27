// Command selfaction runs GitHub Actions workflows on your own machines:
// locally with `run`, as GitHub self-hosted runners with `agent`, from webhooks
// with `server` and `worker`, and with a shared actions/cache backend via `cache`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/docker/go-units"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/broisnees/selfaction/internal/cache"
)

var version = "dev"

func init() {
	// `go install …@version` builds carry their module version; release builds set it with -ldflags.
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
}

var (
	homeDir string
	verbose bool
)

func main() {
	root := &cobra.Command{
		Use:           "selfaction",
		Short:         "Self-hosted, GitHub Actions compatible runners and cache",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(*cobra.Command, []string) {
			log.SetFormatter(&log.TextFormatter{FullTimestamp: true, TimestampFormat: time.TimeOnly})
			if verbose {
				log.SetLevel(log.DebugLevel)
			}
		},
	}
	root.PersistentFlags().StringVar(&homeDir, "home", defaultHome(), "data directory (env SELFACTION_HOME)")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "debug logging")
	root.AddCommand(runCmd(), cacheCmd(), agentCmd(), serverCmd(), workerCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultHome() string {
	if h := os.Getenv("SELFACTION_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".selfaction"
	}
	return filepath.Join(home, ".selfaction")
}

// signalContext ends on Ctrl-C or SIGTERM (what launchd, systemd and docker stop send).
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// policyFlags binds the cache retention limits shared by `cache serve` and `cache gc`.
func policyFlags(cmd *cobra.Command) func() (cache.Policy, error) {
	ttl := cmd.Flags().Duration("ttl", 7*24*time.Hour, "evict caches unused for this long (0 = never)")
	perRepo := cmd.Flags().String("max-repo-size", "10GB", "per-repository size cap, least recently used evicted first (0 = none)")
	total := cmd.Flags().String("max-total-size", "0", "whole-store size cap (0 = none)")
	return func() (cache.Policy, error) {
		p := cache.Policy{TTL: *ttl}
		var err error
		if p.MaxScopeBytes, err = units.RAMInBytes(*perRepo); err != nil {
			return p, fmt.Errorf("--max-repo-size: %w", err)
		}
		if p.MaxTotalBytes, err = units.RAMInBytes(*total); err != nil {
			return p, fmt.Errorf("--max-total-size: %w", err)
		}
		return p, nil
	}
}
