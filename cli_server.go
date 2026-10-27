package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/docker/go-units"
	"github.com/nektos/act/pkg/common"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/broisnees/selfaction/internal/cache"
	"github.com/broisnees/selfaction/internal/github"
	"github.com/broisnees/selfaction/internal/server"
)

func serverCmd() *cobra.Command {
	var (
		addr, externalURL, secret, workerTok string
		ghToken, tokenFile, api, workflows   string
		allowForks, noCache                  bool
	)
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Take GitHub webhooks and queue workflow runs for selfaction workers",
		Long: `The server receives push and pull_request webhooks, reads the repository's
workflows at that commit and queues one job per workflow the event triggers.
Workers (selfaction worker) claim the jobs, run them with the act engine and
stream the log back. Each job shows on its commit as a status that links to
the live log.

Point a repository or organization webhook at <external-url>/webhooks/github
with content type application/json, the push and pull_request events, and the
same secret as --webhook-secret.

The GitHub token reads workflow files and posts commit statuses. Workers also
get it to clone the repository and as each job's GITHUB_TOKEN. A fine-grained
token needs "Contents: read", "Commit statuses: write" and "Pull requests:
read"; a classic token needs "repo".

Authentication: --github-token, --token-file, $SELFACTION_GITHUB_TOKEN,
$GITHUB_TOKEN, then ` + "`gh auth token`" + `.`,
		Example: `  selfaction server --external-url https://ci.example.com --webhook-secret "$SECRET"
  selfaction server --workflows .selfaction/workflows   # when GitHub Actions also runs .github/workflows
  selfaction server jobs
  selfaction server cancel <job id>`,
	}
	policy := policyFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&addr, "addr", ":8080", "listen address")
	f.StringVar(&externalURL, "external-url", "", "public base URL for the webhook, status links and the workers' cache (default http://<this machine's IP>:<port>)")
	f.StringVar(&secret, "webhook-secret", os.Getenv("SELFACTION_WEBHOOK_SECRET"), "webhook secret, required (env SELFACTION_WEBHOOK_SECRET)")
	f.StringVar(&workerTok, "worker-token", os.Getenv("SELFACTION_WORKER_TOKEN"), "token workers authenticate with (default: generated, kept in <home>/server/worker-token)")
	f.StringVar(&ghToken, "github-token", "", "GitHub token")
	f.StringVar(&tokenFile, "token-file", "", "read the GitHub token from this file")
	f.StringVar(&api, "github-api", "https://api.github.com", "GitHub API base; for GitHub Enterprise Server use https://<host>/api/v3")
	f.StringVar(&workflows, "workflows", ".github/workflows", "workflow directory in each repository; when GitHub Actions also runs the repository, use another one such as .selfaction/workflows so jobs don't run twice")
	f.BoolVar(&allowForks, "allow-forks", false, "run pull requests from forks (their code runs on your workers, with the server's GitHub token)")
	f.BoolVar(&noCache, "no-cache", false, "don't serve actions/cache to workers")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if secret == "" {
			return errors.New("--webhook-secret (or $SELFACTION_WEBHOOK_SECRET) is required, or anyone who finds the server could queue jobs")
		}
		p, err := policy()
		if err != nil {
			return err
		}
		ctx, cancel := signalContext()
		defer cancel()
		token, _, err := resolveToken(ctx, ghToken, tokenFile, "github-token")
		if err != nil {
			return err
		}
		dir := filepath.Join(homeDir, "server")
		store, err := server.Open(dir)
		if err != nil {
			return err
		}
		defer store.Close()
		tokenHint := "<worker token>"
		if workerTok == "" {
			path := filepath.Join(dir, "worker-token")
			if workerTok, err = loadOrCreateToken(path); err != nil {
				return err
			}
			tokenHint = `"$(cat ` + path + `)"`
		}
		if externalURL == "" {
			externalURL = defaultExternalURL(addr)
			log.Infof("status links and the cache use %s; pass --external-url to reach them from outside this network", externalURL)
		}
		externalURL = strings.TrimSuffix(externalURL, "/")

		s := &server.Server{
			Store:       store,
			GitHub:      &github.Client{API: strings.TrimSuffix(api, "/"), Token: token},
			Secret:      secret,
			Workflows:   workflows,
			AllowForks:  allowForks,
			ExternalURL: externalURL,
			WorkerToken: workerTok,
			Log:         log.StandardLogger(),
		}
		mux := http.NewServeMux()
		mux.Handle("/", s.Handler())
		if !noCache {
			cacheDir := filepath.Join(dir, "cache")
			cs, err := cache.Open(cacheDir)
			if err != nil {
				return err
			}
			defer cs.Close()
			cacheToken, err := loadOrCreateToken(filepath.Join(cacheDir, "token"))
			if err != nil {
				return err
			}
			c := &cache.Server{Store: cs, Token: cacheToken, ExternalURL: externalURL + "/cache", Log: log.StandardLogger()}
			go c.RunGC(ctx, p, 10*time.Minute)
			mux.Handle("/cache/", http.StripPrefix("/cache", c.Handler()))
			s.CacheURL = externalURL + "/cache/" + cacheToken
		}
		go s.RunReaper(ctx)

		srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		log.Infof("server on %s (store %s), workflows from %s", addr, dir, workflows)
		log.Infof("webhook: %s/webhooks/github (content type application/json, events push and pull_request)", externalURL)
		log.Infof("start workers with: selfaction worker --server %s --token %s", externalURL, tokenHint)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	cmd.AddCommand(serverJobsCmd(), serverCancelCmd())
	return cmd
}

// defaultExternalURL reaches the server from this network, including from job
// containers, which can't use localhost.
func defaultExternalURL(addr string) string {
	host, port, _ := net.SplitHostPort(addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
		if ip := common.GetOutboundIP(); ip != nil {
			host = ip.String()
		}
	}
	return "http://" + net.JoinHostPort(host, port)
}

// workerToken is the --token value or, on the server's own machine, the token the server generated.
func workerToken(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	b, err := os.ReadFile(filepath.Join(homeDir, "server", "worker-token"))
	if err != nil {
		return "", errors.New("no worker token: pass --token or set SELFACTION_WORKER_TOKEN (the server keeps it in <home>/server/worker-token)")
	}
	return strings.TrimSpace(string(b)), nil
}

// apiFlags point the job helpers at a server's worker API.
type apiFlags struct {
	server, token string
}

func (a *apiFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&a.server, "server", "http://localhost:8080", "server base URL")
	cmd.Flags().StringVar(&a.token, "token", os.Getenv("SELFACTION_WORKER_TOKEN"), "worker token (env SELFACTION_WORKER_TOKEN; default: the one in <home>/server/worker-token)")
}

func (a *apiFlags) do(ctx context.Context, method, path string, out any) error {
	token, err := workerToken(a.token)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(a.server, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s %s: %s %s", method, path, res.Status, bytes.TrimSpace(b))
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func serverJobsCmd() *cobra.Command {
	var a apiFlags
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List the server's latest 100 jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var jobs []*server.Job
			if err := a.do(cmd.Context(), "GET", "/api/v1/jobs", &jobs); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSTATUS\tREPO\tWORKFLOW\tEVENT\tCOMMIT\tWORKER\tCREATED")
			for _, j := range jobs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%.7s\t%s\t%s ago\n", j.ID, j.Status, j.Repo, j.Name, j.Event, j.SHA, j.Worker,
					units.HumanDuration(time.Since(j.CreatedAt)))
			}
			return w.Flush()
		},
	}
	a.bind(cmd)
	return cmd
}

func serverCancelCmd() *cobra.Command {
	var a apiFlags
	cmd := &cobra.Command{
		Use:   "cancel <job id>...",
		Short: "Cancel queued or running jobs (a running job stops within seconds)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, ids []string) error {
			for _, id := range ids {
				var j server.Job
				if err := a.do(cmd.Context(), "POST", "/api/v1/jobs/"+id+"/cancel", &j); err != nil {
					return err
				}
				fmt.Printf("%s %s\n", j.ID, j.Status)
			}
			return nil
		},
	}
	a.bind(cmd)
	return cmd
}
