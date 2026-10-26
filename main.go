// Command selfaction runs GitHub Actions workflows on your own machines:
// locally with `run`, as GitHub self-hosted runners with `agent`, and with a
// shared actions/cache backend via `cache`.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var version = "dev"

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
