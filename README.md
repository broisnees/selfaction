# selfaction

Run GitHub Actions on your own machines. It works with your existing workflows and the actions on the Marketplace, and runs on your laptop, a server or in Docker.

**Website, docs and blog: https://selfaction.jotko.workers.dev**

| Command | What it does |
|---|---|
| `selfaction run` | Runs a repo's `.github/workflows` locally in Docker (nektos/act engine), with a persistent `actions/cache` backend and an artifact server. |
| `selfaction agent` | Makes this machine a pool of **GitHub self-hosted runners**. It registers single-use (JIT) runners and runs the official `actions/runner`, so it is 100% compatible. Workflows target it with `runs-on: [self-hosted, <label>]`. |
| `selfaction server` / `selfaction worker` | **CI from webhooks**, without GitHub's runner service. The server takes `push` and `pull_request` webhooks and queues every job of the workflows they trigger; workers on Linux, macOS and Windows run each job with the act engine and report back as commit statuses (or check runs) that link to a live log. Secrets, artifacts and the cache are served by the server. |
| `selfaction cache` | A shared `actions/cache` server for every repo, with per-repo isolation, LRU/TTL eviction, and `ls` / `prune` / `gc` tools. |

Install it on Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/broisnees/selfaction/main/install.sh | sh
```

On Windows, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/broisnees/selfaction/main/install.ps1 | iex
```

Both download the latest [release](https://github.com/broisnees/selfaction/releases) for your system and check it against the release's checksums. With Go 1.26 or newer you can also build it:

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

- **Keep it running:** `selfaction agent install --repo you/app --labels my-mac` (launchd on macOS, `systemd --user` on Linux, a scheduled task on Windows). Logs go to `~/.selfaction/agent.log`.
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

Add a repository (or organization) webhook for `push` and `pull_request` at `https://ci.example.com/webhooks/github`, content type `application/json`, with the same secret. Every push then shows one status per job on the commit, such as `selfaction/ci / test (ubuntu-latest) (push)`, going from Queued to Running to Passed, with a link to the live log.

- **Matching GitHub:** the server reads the workflows at the pushed commit and applies GitHub's `branches`, `tags`, `paths` and pull request `types` filters. Every job of a triggered workflow is queued on its own, once per `strategy.matrix` combination, and released when its `needs` have finished and its `if` holds. `needs.<job>.outputs`, `fail-fast` and `max-parallel` work across workers.
- **Workers:** a worker serves `ubuntu-*` (in Docker), `self-hosted`, `macos-*` on a Mac and `windows-*` on Windows (on the host), anything mapped with `-P`, and `--labels`. A job goes to a worker that carries all of its `runs-on` labels, so one push can build on Linux, macOS, Windows, Android and iOS machines at once. `selfaction worker install` keeps one running as a service.
- **Secrets:** `selfaction server secret set you/app NPM_TOKEN` stores a secret encrypted (AES-256-GCM); jobs of that repository get it as `secrets.NPM_TOKEN`. Fork pull requests never do.
- **Artifacts and cache:** the server is the store for `actions/upload-artifact` / `download-artifact` (v4 and newer) and `actions/cache`, so a job on one worker downloads what a job on another uploaded. The run page lists the artifacts.
- **GitHub App:** `--app-id` and `--app-key` authenticate as an app, with short-lived installation tokens and check runs instead of commit statuses.
- **Jobs:** `selfaction server jobs` lists them, `selfaction server cancel <job or run id>` stops them within seconds.
- **No public address?** `cloudflared tunnel --url http://localhost:8080` gives a temporary one to use as `--external-url`.
- **Differences from GitHub Actions:** only `push` and `pull_request` start runs, pull requests run their head commit, and organization secrets, environments, `vars` and `concurrency` are not supported.

Pull requests from forks are skipped unless you pass `--allow-forks`, since their code would run on your workers with your token. Keep server-mode workflows in their own directory (`--workflows .selfaction/workflows`) if GitHub Actions also runs the repository, so nothing runs twice. [`.selfaction/workflows/platforms.yml`](.selfaction/workflows/platforms.yml) builds the samples in `examples/platforms` on all five platforms; see the [platforms guide](https://selfaction.jotko.workers.dev/docs/guides/platforms/).

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
internal/server        webhooks, trigger filters, run planning (needs, matrix, if), SQLite job queue, worker API,
                       secrets, artifact service, job and run pages, commit statuses and check runs
internal/worker        claims jobs, fetches the commit, runs the one job with act, streams the log
internal/github        REST client and GitHub App authentication, shared by the agent and the server
internal/service       login services: launchd, systemd --user, Windows scheduled tasks
examples/cache-demo    workflow that exercises actions/cache end to end
examples/platforms     samples the platforms workflow builds: Go CLI, Android app, iOS package
images/android         act job image with the Android SDK
scripts/release.sh     release archives for every platform (run by .github/workflows/release.yml on a tag)
install.sh, .ps1       installers for Linux/macOS and Windows
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

- **Server mode:** Gitea/Forgejo webhooks, re-running jobs from the Checks tab, and a fresh Tart VM per macOS job.
- **AI:**
  - explanations of failed jobs on PRs
  - auto-fix PRs
  - flaky-test detection
  - cache-key advice
  - workflow generation and migration
- Web UI, Helm chart, S3 storage for the cache and artifacts.
