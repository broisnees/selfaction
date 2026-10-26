# selfaction

Run GitHub Actions on your own machines. It works with your existing workflows and the actions on the Marketplace, and runs on your laptop, a server or in Docker.

| Command | What it does |
|---|---|
| `selfaction run` | Runs a repo's `.github/workflows` locally in Docker (nektos/act engine), with a persistent `actions/cache` backend and an artifact server. |
| `selfaction agent` | Makes this machine a pool of **GitHub self-hosted runners**. It registers single-use (JIT) runners and runs the official `actions/runner`, so it is 100% compatible. Workflows target it with `runs-on: [self-hosted, <label>]`. |
| `selfaction cache` | A shared `actions/cache` server for every repo, with per-repo isolation, LRU/TTL eviction, and `ls` / `prune` / `gc` tools. |

Build it (Go 1.26+):

```sh
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
3. Wipes the workspace, diagnostics and credentials. The `actions/setup-*` tool cache is kept.
4. Repeats.

Runners are unregistered when the agent stops.

- **Keep it running:** `selfaction agent install --repo you/app --labels my-mac` (launchd on macOS, `systemd --user` on Linux). Logs go to `~/.selfaction/agent.log`.
- **Isolated jobs:** `--mode docker` runs each job in a fresh `ghcr.io/actions/actions-runner` container. Add `--docker-socket` if jobs use `container:`, `services:` or `docker build`.
- **Organization:** `--org your-org` instead of `--repo`.
- **GitHub Enterprise Server:** `--github-api https://ghe.example.com/api/v3`.
- **Token:** `--token`, `--token-file`, `$GITHUB_TOKEN`, or your `gh` login. Repo runners need repo admin (classic `repo` scope or fine-grained "Administration: write"). Org runners need `admin:org`.

Only attach runners to repositories you trust. On a public repo, a fork's pull request can run code on your machine.

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
examples/cache-demo    workflow that exercises actions/cache end to end
```

## Roadmap

- **Server mode:** a GitHub App that takes webhooks, a job queue, and agents that pull jobs and run them with the act engine, with results reported back through the Checks API. This runs workflows without GitHub's runner service and works with Gitea/Forgejo too.
- **AI:**
  - explanations of failed jobs on PRs
  - auto-fix PRs
  - flaky-test detection
  - cache-key advice
  - workflow generation and migration
- Web UI, Helm chart, Windows host mode, S3 storage for the cache.
