package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/docker/go-units"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/broisnischal/selfaction/internal/cache"
)

func cacheCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Serve, inspect and clean the actions/cache store",
	}
	cmd.PersistentFlags().StringVar(&dir, "dir", "", "cache store directory (default <home>/cache)")
	open := func() (*cache.Store, error) {
		if dir == "" {
			dir = filepath.Join(homeDir, "cache")
		}
		return cache.Open(dir)
	}
	cmd.AddCommand(cacheServeCmd(open, &dir), cacheListCmd(open), cacheRmCmd(open))
	return cmd
}

func cacheServeCmd(open func() (*cache.Store, error), dir *string) *cobra.Command {
	var addr, token, externalURL string
	var gcEvery time.Duration
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run a shared cache server for `selfaction run` and act-based runners",
	}
	policy := policyFlags(cmd)
	cmd.Flags().StringVar(&addr, "addr", ":7979", "listen address")
	cmd.Flags().StringVar(&token, "token", os.Getenv("SELFACTION_CACHE_TOKEN"), "access token embedded in cache URLs (default: generated, kept in <dir>/token)")
	cmd.Flags().StringVar(&externalURL, "external-url", "", "public base URL for download links, e.g. https://cache.example.com (default: the Host the job used)")
	cmd.Flags().DurationVar(&gcEvery, "gc-interval", 10*time.Minute, "how often to apply the retention policy")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		p, err := policy()
		if err != nil {
			return err
		}
		store, err := open()
		if err != nil {
			return err
		}
		defer store.Close()
		if token == "" {
			if token, err = loadOrCreateToken(filepath.Join(*dir, "token")); err != nil {
				return err
			}
		}
		ctx, cancel := signalContext()
		defer cancel()
		s := &cache.Server{Store: store, Token: token, ExternalURL: externalURL, Log: log.StandardLogger()}
		go s.RunGC(ctx, p, gcEvery)
		srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		_, port, _ := net.SplitHostPort(addr)
		log.Infof("cache server on %s (store %s)", addr, *dir)
		log.Infof("use it with: selfaction run --cache-url http://<this-host>:%s/%s", port, token)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	return cmd
}

func loadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	return token, os.WriteFile(path, []byte(token+"\n"), 0o600)
}

func filterFlags(cmd *cobra.Command) func() cache.Filter {
	repo := cmd.Flags().String("repo", "", "only this repository (owner/repo)")
	key := cmd.Flags().String("key", "", "only keys starting with this prefix")
	unused := cmd.Flags().Duration("unused-for", 0, "only caches not used for this long, e.g. 72h")
	return func() cache.Filter {
		f := cache.Filter{KeyPrefix: *key, UnusedFor: *unused}
		if *repo != "" {
			f.Scope = cache.ScopeOf(*repo)
		}
		return f
	}
}

func cacheListCmd(open func() (*cache.Store, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "ls", Short: "List cache entries"}
	filter := filterFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		store, err := open()
		if err != nil {
			return err
		}
		defer store.Close()
		entries, err := store.List(cmd.Context(), filter())
		if err != nil {
			return err
		}
		printEntries(entries)
		return nil
	}
	return cmd
}

func cacheRmCmd(open func() (*cache.Store, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>...",
		Short: "Delete cache entries by id",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ids := make([]int64, len(args))
			for i, a := range args {
				id, err := strconv.ParseInt(a, 10, 64)
				if err != nil {
					return fmt.Errorf("invalid id %q", a)
				}
				ids[i] = id
			}
			store, err := open()
			if err != nil {
				return err
			}
			defer store.Close()
			return store.Delete(cmd.Context(), ids...)
		},
	}
}

func printEntries(entries []cache.Entry) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tREPO\tKEY\tSIZE\tCREATED\tLAST USED")
	for _, e := range entries {
		key := e.Key
		if !e.Complete {
			key += " (uploading)"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s ago\t%s ago\n", e.ID, e.Scope, key, units.HumanSize(float64(e.Size)),
			units.HumanDuration(time.Since(e.CreatedAt)), units.HumanDuration(time.Since(e.UsedAt)))
	}
	w.Flush()
}
