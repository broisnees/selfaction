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
	"strconv"
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
		appKey                               string
		appID                                int64
		allowForks, noCache, noArtifacts     bool
		artifactRetention                    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Take GitHub webhooks and queue workflow runs for selfaction workers",
		Long: `The server receives push and pull_request webhooks, reads the repository's
workflows at that commit and queues every job of each workflow the event
triggers, one per matrix combination. A job is released once the jobs it needs
have finished. Workers (selfaction worker) claim jobs whose runs-on labels they
serve, run them with the act engine and stream the log back. Each job shows on
its commit as a status (or, as a GitHub App, a check run) that links to the
live log. The server also serves actions/cache and actions/upload-artifact to
its workers, and keeps encrypted repository secrets for their jobs.

Point a repository or organization webhook at <external-url>/webhooks/github
with content type application/json, the push and pull_request events, and the
same secret as --webhook-secret.

The GitHub token reads workflow files and posts commit statuses. Workers also
get it to clone the repository and as each job's GITHUB_TOKEN. A fine-grained
token needs "Contents: read", "Commit statuses: write" and "Pull requests:
read"; a classic token needs "repo".

Authentication: --app-id and --app-key for a GitHub App, else --github-token,
--token-file, $SELFACTION_GITHUB_TOKEN, $GITHUB_TOKEN, then ` + "`gh auth token`" + `.`,
		Example: `  selfaction server --external-url https://ci.example.com --webhook-secret "$SECRET"
  selfaction server --workflows .selfaction/workflows   # when GitHub Actions also runs .github/workflows
  selfaction server --app-id 123456 --app-key ~/selfaction.private-key.pem   # as a GitHub App, with check runs
  selfaction server jobs
  selfaction server cancel <job or run id>
  echo -n "$NPM_TOKEN" | selfaction server secret set you/app NPM_TOKEN`,
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
	f.BoolVar(&noArtifacts, "no-artifacts", false, "don't serve actions/upload-artifact; each worker keeps its jobs' artifacts")
	f.DurationVar(&artifactRetention, "artifact-retention", 90*24*time.Hour, "delete artifacts after this long (a job's retention-days can only shorten it)")
	f.Int64Var(&appID, "app-id", envInt("SELFACTION_APP_ID"), "GitHub App id: authenticate as the app and report check runs (env SELFACTION_APP_ID)")
	f.StringVar(&appKey, "app-key", os.Getenv("SELFACTION_APP_KEY"), "the GitHub App's private key file, .pem (env SELFACTION_APP_KEY)")
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
		var app *github.App
		var token string
		switch {
		case appID != 0 || appKey != "":
			if appID == 0 || appKey == "" {
				return errors.New("a GitHub App needs both --app-id and --app-key")
			}
			if app, err = github.LoadApp(appID, appKey, api); err != nil {
				return err
			}
		default:
			if token, _, err = resolveToken(ctx, ghToken, tokenFile, "github-token"); err != nil {
				return err
			}
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

		sealer, err := server.LoadSealer(filepath.Join(dir, "secrets.key"))
		if err != nil {
			return err
		}
		s := &server.Server{
			Store:             store,
			GitHub:            &github.Client{API: strings.TrimSuffix(api, "/"), Token: token},
			App:               app,
			Secret:            secret,
			Workflows:         workflows,
			AllowForks:        allowForks,
			ExternalURL:       externalURL,
			WorkerToken:       workerTok,
			Sealer:            sealer,
			ArtifactRetention: artifactRetention,
			Log:               log.StandardLogger(),
		}
		if !noArtifacts {
			s.Artifacts = filepath.Join(dir, "artifacts")
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
		if app != nil {
			log.Infof("authenticating as GitHub App %d; results are check runs", appID)
		}
		log.Infof("server on %s (store %s), workflows from %s", addr, dir, workflows)
		log.Infof("webhook: %s/webhooks/github (content type application/json, events push and pull_request)", externalURL)
		log.Infof("start workers with: selfaction worker --server %s --token %s", externalURL, tokenHint)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	cmd.AddCommand(serverJobsCmd(), serverCancelCmd(), serverSecretCmd())
	return cmd
}

func envInt(name string) int64 {
	n, _ := strconv.ParseInt(os.Getenv(name), 10, 64)
	return n
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

// errNotFound is a 404 from the server.
var errNotFound = errors.New("not found")

func (a *apiFlags) do(ctx context.Context, method, path string, out any) error {
	return a.send(ctx, method, path, nil, out)
}

func (a *apiFlags) send(ctx context.Context, method, path string, body io.Reader, out any) error {
	token, err := workerToken(a.token)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(a.server, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, errNotFound)
	}
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s %s: %s %s", method, path, res.Status, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
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
			fmt.Fprintln(w, "ID\tSTATUS\tREPO\tWORKFLOW\tJOB\tEVENT\tCOMMIT\tWORKER\tRUN\tCREATED")
			for _, j := range jobs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%.7s\t%s\t%s\t%s ago\n", j.ID, j.Status, j.Repo, j.WorkflowName, j.Name, j.Event, j.SHA, j.Worker,
					j.RunID, units.HumanDuration(time.Since(j.CreatedAt)))
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
		Use:   "cancel <job or run id>...",
		Short: "Cancel jobs, or every unfinished job of a run (a running job stops within seconds)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, ids []string) error {
			for _, id := range ids {
				var j server.Job
				err := a.do(cmd.Context(), "POST", "/api/v1/jobs/"+id+"/cancel", &j)
				if errors.Is(err, errNotFound) {
					var jobs []*server.Job
					if err := a.do(cmd.Context(), "POST", "/api/v1/runs/"+id+"/cancel", &jobs); err != nil {
						return err
					}
					for _, j := range jobs {
						fmt.Printf("%s %s %s\n", j.ID, j.Status, j.Name)
					}
					continue
				}
				if err != nil {
					return err
				}
				fmt.Printf("%s %s %s\n", j.ID, j.Status, j.Name)
			}
			return nil
		},
	}
	a.bind(cmd)
	return cmd
}

func serverSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage the repository secrets the server passes to jobs as secrets.*",
		Long: `Secrets are stored encrypted (AES-256-GCM, key in <home>/server/secrets.key)
and passed to the jobs of that repository as secrets.<NAME>. Like on GitHub,
names are case-insensitive, values are masked in job logs, and pull requests
from forks get none. Every command takes --server and --token, the worker token.`,
	}
	repoArg := func(args []string) (string, error) {
		if strings.Count(args[0], "/") != 1 {
			return "", fmt.Errorf("want owner/repo, got %q", args[0])
		}
		return args[0], nil
	}

	var set apiFlags
	var body string
	setCmd := &cobra.Command{
		Use:     "set <owner/repo> <NAME>",
		Short:   "Set a secret from --body or standard input",
		Example: "  selfaction server secret set you/app NPM_TOKEN --body \"$NPM_TOKEN\"\n  selfaction server secret set you/app SIGNING_KEY < key.p12.b64",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := repoArg(args)
			if err != nil {
				return err
			}
			value := []byte(body)
			if !cmd.Flags().Changed("body") {
				if value, err = io.ReadAll(io.LimitReader(os.Stdin, 48<<10+1)); err != nil {
					return err
				}
			}
			if err := set.send(cmd.Context(), "PUT", "/api/v1/repos/"+repo+"/secrets/"+args[1], bytes.NewReader(value), nil); err != nil {
				return err
			}
			fmt.Printf("set %s for %s\n", strings.ToUpper(args[1]), repo)
			return nil
		},
	}
	set.bind(setCmd)
	setCmd.Flags().StringVarP(&body, "body", "b", "", "the value (default: read standard input)")

	var ls apiFlags
	lsCmd := &cobra.Command{
		Use:   "ls <owner/repo>",
		Short: "List a repository's secrets (names only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := repoArg(args)
			if err != nil {
				return err
			}
			var infos []server.SecretInfo
			if err := ls.do(cmd.Context(), "GET", "/api/v1/repos/"+repo+"/secrets", &infos); err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tUPDATED")
			for _, s := range infos {
				fmt.Fprintf(w, "%s\t%s ago\n", s.Name, units.HumanDuration(time.Since(s.UpdatedAt)))
			}
			return w.Flush()
		},
	}
	ls.bind(lsCmd)

	var rm apiFlags
	rmCmd := &cobra.Command{
		Use:   "rm <owner/repo> <NAME>...",
		Short: "Delete secrets",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := repoArg(args)
			if err != nil {
				return err
			}
			for _, name := range args[1:] {
				if err := rm.do(cmd.Context(), "DELETE", "/api/v1/repos/"+repo+"/secrets/"+name, nil); err != nil {
					return err
				}
				fmt.Printf("deleted %s\n", strings.ToUpper(name))
			}
			return nil
		},
	}
	rm.bind(rmCmd)

	cmd.AddCommand(setCmd, lsCmd, rmCmd)
	return cmd
}
