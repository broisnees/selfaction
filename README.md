# selfaction

Run GitHub Actions on your own machines. It works with your existing workflows and the actions on the Marketplace, and runs on your laptop, a server or in Docker.

**Website, docs and blog: https://selfaction.jotko.workers.dev**

| Command | What it does |
|---|---|
| `selfaction run` | Runs a repo's `.github/workflows` locally in Docker (nektos/act engine), with a persistent `actions/cache` backend and an artifact server. |
| `selfaction agent` | Makes this machine a pool of **GitHub self-hosted runners**. It registers single-use (JIT) runners and runs the official `actions/runner`, so it is 100% compatible. Workflows target it with `runs-on: [self-hosted, <label>]`. |
| `selfaction server` / `selfaction worker` | **CI from webhooks**, without GitHub's runner service. The server takes `push` and `pull_request` webhooks and queues the workflows they trigger; workers run them with the act engine and report back as commit statuses that link to a live log. |
| `selfaction cache` | A shared `actions/cache` server for every repo, with per-repo isolation, LRU/TTL eviction, and `ls` / `prune` / `gc` tools. |

Install it (Go 1.26+):

```sh
go install github.com/broisnees/selfaction@latest
# or, from a clone:
go build -o bin/selfaction .
```

## Run workflows locally

```sh
cd your-repo
selfaction run --list              # what would run
selfaction run                     # the workflows' only event, else push
selfaction run pull_request -j test
selfaction run -s NPM_TOKEN        # secret from your env; GITHUB_TOKEN comes from `gh auth token`
selfaction run -P ubuntu-latest=catthehacker/ubuntu:full-latest   # bigger, closer-to-GitHub image
```

`ubuntu-*` jobs run in `catthehacker/ubuntu:act-*` images. On a Mac, `macos-*` and `self-hosted` jobs run directly on the host. `actions/cache` (v4–v6) works with no setup: archives go to `~/.selfaction/cache` and are scoped by `owner/repo`, so the second run restores them.

## Use your PC as a GitHub Actions runner

```sh
selfaction agent start --repo you/app --labels my-mac --slots 2
```

```yaml
jobs:
  build:
    runs-on: [self-hosted, my-mac]
```

Each slot does the same cycle:
1. Registers a just-in-time runner. It has at most one job and the credential is passed through the environment, not argv.
2. Runs that one job.
3. Wipes the workspace, diagnostics and credentials. The `actions/setup-*` tool cache and the slot's own home directory are kept.
4. Repeats.

Host-mode jobs run with a per-slot `HOME`, so their caches and dotfiles stay apart from yours (`--share-home` turns that off). Runners that GitHub reports offline, for example after the machine slept, are restarted. Runners are unregistered when the agent stops.

- **Keep it running:** `selfaction agent install --repo you/app --labels my-mac` (launchd on macOS, `systemd --user` on Linux). Logs go to `~/.selfaction/agent.log`.
- **Isolated jobs:** `--mode docker` runs each job in a fresh `ghcr.io/actions/actions-runner` container. Add `--docker-socket` if jobs use `container:`, `services:` or `docker build`.
- **Organization:** `--org your-org` instead of `--repo`.
- **GitHub Enterprise Server:** `--github-api https://ghe.example.com/api/v3`.
- **Token:** `--token`, `--token-file`, `$GITHUB_TOKEN`, or your `gh` login. Repo runners need repo admin (classic `repo` scope or fine-grained "Administration: write"). Org runners need `admin:org`.

Only attach runners to repositories you trust. On a public repo, target self-hosted runners only from `push` to `main` or `workflow_dispatch`, never from `pull_request`, so a fork's code cannot run on your machine.

## Run workflows from webhooks

```sh
export SELFACTION_WEBHOOK_SECRET=$(openssl rand -hex 32)
selfaction server --external-url https://ci.example.com --workflows .selfaction/workflows
selfaction worker --server https://ci.example.com --token <worker token> --slots 2
```

Add a repository (or organization) webhook for `push` and `pull_request` at `https://ci.example.com/webhooks/github`, content type `application/json`, with the same secret. Every push then shows `selfaction/<workflow> (push)` on the commit, going from Queued to Running to Passed, with a link to the live log.

- **Matching GitHub:** the server reads the workflows at the pushed commit and applies GitHub's `branches`, `tags`, `paths` and pull request `types` filters. Each triggered workflow becomes one job, claimed by a worker that serves all of its `runs-on` labels.
- **Workers:** a worker serves `ubuntu-*` (in Docker), `self-hosted` (on the host), anything mapped with `-P`, and `--labels`. The worker token is generated on the server's first start and kept in `~/.selfaction/server/worker-token`.
- **Shared cache:** the server serves `actions/cache` under `/cache/` to every worker, with the same retention flags as `cache serve`.
- **Jobs:** `selfaction server jobs` lists them, `selfaction server cancel <id>` stops one within seconds.
- **No public address?** `cloudflared tunnel --url http://localhost:8080` gives a temporary one to use as `--external-url`.
- **Differences from GitHub Actions:** only `push` and `pull_request` start jobs, pull requests run their head commit, and jobs get `GITHUB_TOKEN` (the server's token) but no repository secrets.

Pull requests from forks are skipped unless you pass `--allow-forks`, since their code would run on your workers with your token. Keep server-mode workflows in their own directory (`--workflows .selfaction/workflows`) if GitHub Actions also runs the repository, so nothing runs twice.

## Shared cache server

```sh
selfaction cache serve                      # :7979, token kept in ~/.selfaction/cache/token
selfaction run --cache-url http://cache.lan:7979/<token>
```

It speaks the `actions/cache` v1 HTTP API, the one act-based runners and GHES use. Jobs get `ACTIONS_CACHE_URL=<base>/<token>/<owner.repo>/`, so every repository has its own namespace.

Retention:

| Flag | Default |
|---|---|
| `--ttl` | `168h` |
| `--max-repo-size` | `10GB` (least recently used first) |
| `--max-total-size` | none |

Abandoned uploads and orphaned files are collected automatically.

Jobs on the official runner (`selfaction agent`) keep using GitHub's own cache service. This server serves `selfaction run` and other act-based runners.

## Cleaning up

```sh
selfaction cache ls --repo you/app
selfaction cache prune --repo you/app --key node-modules-   # --dry-run to preview
selfaction cache prune --unused-for 72h
selfaction cache gc --max-total-size 50GB                    # apply limits now
selfaction agent clean                  # workspaces, diagnostics, stale credentials
selfaction agent clean --tools          # + setup-* tool cache
selfaction agent clean --runners        # + runner installs (re-downloaded on start)
selfaction agent clean --docker         # + docker system prune of objects older than 72h (whole Docker host)
```

## Docker

```sh
docker compose up -d                                                    # cache server on :7979
GITHUB_TOKEN=... GITHUB_REPO=you/app docker compose --profile agent up -d   # + 2 docker-mode runners
```

## Layout

```
main.go, cli_*.go      CLI (cobra)
internal/run           act-based local execution
internal/cache         cache store (SQLite index + blob files) and HTTP API
internal/agent         JIT runner pool, runner download/verify, service install
internal/server        webhooks, trigger filters, SQLite job queue, worker API, job pages, commit statuses
internal/worker        claims jobs, fetches the commit, runs it with act, streams the log
internal/github        REST client shared by the agent and the server
examples/cache-demo    workflow that exercises actions/cache end to end
site/                  website: Astro, Starlight and Tailwind, deployed to Cloudflare Workers
```

## Website

The site in `site/` holds the landing page, docs, blog and the record of real runs.

```sh
cd site
npm ci
npm run dev       # http://localhost:4321
npm run deploy    # build and deploy with wrangler (needs `wrangler login`)
```

On every push to `main` that touches `site/`, the `site` workflow builds it on a selfaction runner and deploys it. Add a `CLOUDFLARE_API_TOKEN` secret (the "Edit Cloudflare Workers" token template) and a `CLOUDFLARE_ACCOUNT_ID` variable to enable the deploy step.

## Roadmap

- **Server mode:** GitHub App authentication with check runs instead of commit statuses, and Gitea/Forgejo webhooks.
- **AI:**
  - explanations of failed jobs on PRs
  - auto-fix PRs
  - flaky-test detection
  - cache-key advice
  - workflow generation and migration
- Web UI, Helm chart, Windows host mode, S3 storage for the cache.
