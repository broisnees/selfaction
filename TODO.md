# TODO

## Step 3: server mode (webhooks, job queue, workers)

Done and pushed: `internal/server` (webhook receiver, trigger filters, SQLite queue, worker API, job pages, commit statuses) and `internal/worker` (claim, shallow fetch, act run, log streaming, cancel). Both have tests (`go test -race ./...`). Nothing calls them from the CLI yet.

### 1. CLI commands

- [ ] `selfaction server` in a new `cli_server.go`. Builds a `server.Server`, starts `RunReaper`, and serves `Handler()`.
  - `--addr` (default `:8080`)
  - `--external-url`: public base URL, used in status links
  - `--webhook-secret`: required; also read from `$SELFACTION_WEBHOOK_SECRET`. Refuse to start without it.
  - `--worker-token`: generated on first start and kept in `<home>/server/worker-token` (copy `loadOrCreateToken` in `cli_cache.go`)
  - `--github-token` / `--token-file`: fall back to `$GITHUB_TOKEN`, then the gh CLI login (see `agentFlags.resolveToken`)
  - `--github-api` (default `https://api.github.com`)
  - `--workflows` (default `.github/workflows`). Suggest `.selfaction/workflows` when GitHub Actions also runs the repo, so jobs don't run twice.
  - `--allow-forks` (default off)
  - store in `<home>/server`
- [ ] `selfaction worker` in a new `cli_worker.go`. Builds a `worker.Worker`.
  - `--server`, `--token`, `--name` (hostname, as `runnerName` in `cli_agent.go`), `--slots`
  - `--labels`: default to the keys of `run.DefaultPlatforms()` plus any extras
  - `-P/--platform`, `--container-architecture`, `--job-timeout`
  - data in `<home>/worker`
- [ ] `server jobs` / `server cancel <id>` helpers, or document `GET /api/v1/jobs` and `POST /api/v1/jobs/{id}/cancel` (Bearer worker token).

### 2. Shared cache for workers

- [ ] In `selfaction server`, open a `cache.Store` at `<home>/server/cache` and mount `cache.Server.Handler()` under `/cache/` with `http.StripPrefix`.
- [ ] Set `Server.CacheURL` to `<external-url>/cache/<cache-token>`. Workers already pass `Claim.CacheURL` to `run.Options.CacheURL`.
- [ ] Run `cache.Server.RunGC` with the default policy, and add `--no-cache`.

### 3. Worker fix

- [ ] In `worker.work`, when the server cancelled the job (the `cancelled` channel closed), skip `POST /finish`. The job is already final on the server, so that post only produces a 409 in the worker log (visible in `TestWorkerRunsClaimedJobs`).

### 4. Real end-to-end test

- [ ] Run `selfaction server --external-url <tunnel> --webhook-secret …` and `selfaction worker --server http://localhost:8080 --token …`.
- [ ] Expose the server: `cloudflared tunnel --url http://localhost:8080` prints a `trycloudflare.com` URL. Use it as `--external-url`.
- [ ] Add a repository webhook for `push` and `pull_request`, content type `application/json`, with the secret:
  `gh api repos/broisnees/selfaction/hooks -f name=web -F active=true -f 'events[]=push' -f 'events[]=pull_request' -f config[url]=<tunnel>/webhooks/github -f config[content_type]=json -f config[secret]=…`
- [ ] Add a workflow under `.selfaction/workflows/` (and run the server with `--workflows .selfaction/workflows`), push, and check:
  - the commit shows `selfaction/<name> (push)` going from pending to success
  - the status link opens the live log page
  - a cancel through the API stops the job
  - a second push gets a cache hit through the server's cache
- [ ] Delete the webhook afterwards if the tunnel URL is temporary.

### 5. Docs, site and extras

- [ ] Docs page `site/src/content/docs/docs/guides/server.mdx`: how it works, setup, flags, security (fork PRs are skipped by default; job log URLs are unguessable; workers hold the server's GitHub token).
- [ ] CLI reference entries in `site/src/content/docs/docs/reference/cli.mdx`.
- [ ] README section, and remove "Server mode" from the roadmap there and on the site.
- [ ] Blog post once the real end-to-end run works.
- [ ] Optional: GitHub App authentication.
  - Sign a JWT (RS256) with the app's private key and exchange it at `/app/installations/{id}/access_tokens`, using `installation.id` from the delivery.
  - That gives short-lived per-repository tokens instead of one personal token, and enables the Checks API.

## Other open items

- [ ] Dependabot alert #24 / PR #3 (`moby/go-archive` 0.3.x): it breaks act v0.2.89 (`archive.Uncompressed` was removed). act only creates tar archives, and the advisory is about extracting them, so the vulnerable path isn't used. Dismiss the alert as "vulnerable code not used", or upgrade when an act release supports go-archive 0.3.
- [ ] Add a `CLOUDFLARE_API_TOKEN` repository secret (Cloudflare's "Edit Cloudflare Workers" token template) so the `site` workflow deploys on every push to `main`.
- [ ] GitHub billing lock: jobs on GitHub-hosted runners fail until it's fixed; self-hosted and server-mode jobs are unaffected.
- [ ] The agent service on the MacBook (`dev.selfaction.agent`, label `selfaction-local`) keeps taking jobs while that machine is on.
