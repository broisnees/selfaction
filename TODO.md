# TODO

## Step 3: server mode (webhooks, job queue, workers)

Done and pushed: `internal/server` (webhook receiver, trigger filters, SQLite queue, worker API, job pages, commit statuses) and `internal/worker` (claim, shallow fetch, act run, log streaming, cancel). Both have tests (`go test -race ./...`). `selfaction server` and `selfaction worker` run them, with the shared cache mounted at `/cache/`; checked locally against a fake GitHub API (cache hit on the second push, cancel through `server cancel`).

### 1. CLI commands

- [x] `selfaction server` in a new `cli_server.go`. Builds a `server.Server`, starts `RunReaper`, and serves `Handler()`.
  - `--addr` (default `:8080`)
  - `--external-url`: public base URL, used in status links
  - `--webhook-secret`: required; also read from `$SELFACTION_WEBHOOK_SECRET`. Refuse to start without it.
  - `--worker-token`: generated on first start and kept in `<home>/server/worker-token` (copy `loadOrCreateToken` in `cli_cache.go`)
  - `--github-token` / `--token-file`: fall back to `$GITHUB_TOKEN`, then the gh CLI login (see `agentFlags.resolveToken`)
  - `--github-api` (default `https://api.github.com`)
  - `--workflows` (default `.github/workflows`). Suggest `.selfaction/workflows` when GitHub Actions also runs the repo, so jobs don't run twice.
  - `--allow-forks` (default off)
  - store in `<home>/server`
- [x] `selfaction worker` in a new `cli_worker.go`. Builds a `worker.Worker`.
  - `--server`, `--token`, `--name` (hostname, as `runnerName` in `cli_agent.go`), `--slots`
  - `--labels`: default to the keys of `run.DefaultPlatforms()` plus any extras
  - `-P/--platform`, `--container-architecture`, `--job-timeout`
  - data in `<home>/worker`
- [x] `server jobs` / `server cancel <id>` helpers, or document `GET /api/v1/jobs` and `POST /api/v1/jobs/{id}/cancel` (Bearer worker token).

### 2. Shared cache for workers

- [x] In `selfaction server`, open a `cache.Store` at `<home>/server/cache` and mount `cache.Server.Handler()` under `/cache/` with `http.StripPrefix`.
- [x] Set `Server.CacheURL` to `<external-url>/cache/<cache-token>`. Workers already pass `Claim.CacheURL` to `run.Options.CacheURL`.
- [x] Run `cache.Server.RunGC` with the default policy, and add `--no-cache`.

### 3. Worker fix

- [x] In `worker.work`, when the server cancelled the job (the `cancelled` channel closed), skip `POST /finish`. The job is already final on the server, so that post only produces a 409 in the worker log (visible in `TestWorkerRunsClaimedJobs`).
- [x] Found during this: a worker stopped with Ctrl-C posted `/finish` with its cancelled context, so the job stayed running until the reaper failed it. `/finish` now gets its own deadline (`TestStoppingTheWorkerCancelsItsJobs`).

### 4. Real end-to-end test

- [x] Run `selfaction server --external-url <tunnel> --webhook-secret …` and `selfaction worker --server http://localhost:8080 --token …`.
- [x] Expose the server: `cloudflared tunnel --url http://localhost:8080` prints a `trycloudflare.com` URL. Use it as `--external-url`.
- [x] Add a repository webhook for `push` and `pull_request`, content type `application/json`, with the secret:
  `gh api repos/broisnees/selfaction/hooks -f name=web -F active=true -f 'events[]=push' -f 'events[]=pull_request' -f config[url]=<tunnel>/webhooks/github -f config[content_type]=json -f config[secret]=…`
- [x] Add a workflow under `.selfaction/workflows/` (and run the server with `--workflows .selfaction/workflows`), push, and check:
  - the commit shows `selfaction/<name> (push)` going from pending to success
  - the status link opens the live log page
  - a cancel through the API stops the job
  - a second push gets a cache hit through the server's cache
- [x] Delete the webhook afterwards if the tunnel URL is temporary.

Done on 2026-10-27 against this repository, through a quick tunnel: on 0e50436 `selfaction/ci (push)` went Queued, Running, Passed in 1m 48s and saved a 312 MB module cache through the server; on ebd3875 the cache was restored (`cache-hit=true`) and `server cancel` stopped the job 4 s later, with the status set to Cancelled.

### 5. Docs, site and extras

- [x] Docs page `site/src/content/docs/docs/guides/server.mdx`: how it works, setup, flags, security (fork PRs are skipped by default; job log URLs are unguessable; workers hold the server's GitHub token).
- [x] CLI reference entries in `site/src/content/docs/docs/reference/cli.mdx`.
- [x] README section, and remove "Server mode" from the roadmap there and on the site.
- [x] Blog post once the real end-to-end run works (`site/src/content/blog/server-mode-first-real-push.md`, committed, not deployed).
- [ ] Optional: GitHub App authentication.
  - Sign a JWT (RS256) with the app's private key and exchange it at `/app/installations/{id}/access_tokens`, using `installation.id` from the delivery.
  - That gives short-lived per-repository tokens instead of one personal token, and enables the Checks API.

## Step 4: build for every platform, from any computer

Goal: one push builds for Linux, macOS, Windows, Android and iOS, and the server, a worker or `selfaction run` works on a Linux, Mac or Windows machine. The OS a job needs decides which worker runs it. Apple's license only allows macOS on Apple hardware, so macOS and iOS jobs always land on a Mac worker. "From any computer" means triggering and watching them from anywhere while the server routes each job to a worker that can run it.

Where it stands (2026-10-27):

- `go build` succeeds for linux, darwin and windows on amd64 and arm64, but there are no release binaries; installing needs Go.
- `run` and `worker` map `ubuntu-*` to Docker images, and `macos-*` to the host only when running on a Mac. There is no `windows-*` label.
- `agent` host mode downloads the runner for `osx` and `linux` only; on Windows only `--mode docker` works. `agent install` knows launchd and systemd, not Windows services.
- Server mode claims a whole workflow as one job, and the worker must carry every `runs-on` label in it. A workflow with a macOS, a Windows and a Linux job can never be claimed.
- Server-mode jobs get no secrets, so signing (Apple certificates and provisioning profiles, Android keystores, Windows code signing) can't work yet.
- Artifacts uploaded by a worker's job stay on that worker (`<home>/worker/artifacts/<job id>`); nothing brings them back to the server.

### 1. Install and run on any computer

- [ ] Release workflow: on a tag, build linux, darwin and windows for amd64 and arm64, and attach the archives and checksums to a GitHub release.
- [ ] Install script for Linux and macOS, and a zip (later scoop or winget) for Windows. Update the install guide.
- [ ] Test `run`, `server`, `worker` and `cache` on Windows and fix what breaks: paths, stopping (Ctrl-C and service stop), Docker Desktop.
- [ ] `agent install` and a worker service on Windows (a Windows service or a scheduled task).

### 2. Split server-mode jobs per workflow job

- [ ] Queue each job of a workflow on its own, with its own `runs-on` labels, and release a job once everything in its `needs` has passed. act already runs a single job (`run.Options.Job`); find out how to feed `needs.<job>.outputs` from jobs that ran on other workers.
- [ ] Expand `strategy.matrix` (for example `os: [ubuntu-latest, macos-latest, windows-latest]`) into separate jobs before they are claimed.
- [ ] One commit status per job, such as `selfaction/build (macos-latest) (push)`.

### 3. Platforms

- [ ] **Linux**: amd64 works in Docker. Test arm64 on an amd64 host with `--container-architecture linux/arm64` and QEMU (binfmt).
- [ ] **macOS**: a Mac worker runs `macos-*` jobs on the host. Check what host-mode jobs share between runs (HOME, tool caches); the agent gives each slot its own HOME. Look at Tart VMs on Apple silicon for jobs that need isolation.
- [ ] **iOS**: on a Mac worker with Xcode (`--labels xcode`). Start with an unsigned simulator build with `xcodebuild`, then a signed archive once secrets exist.
- [ ] **Windows**: `windows-latest` and `windows-2022` labels that run on the host of a Windows worker. Check act's host mode on Windows with the `pwsh`, `powershell` and `cmd` shells.
- [ ] **Android**: a Linux image with the Android SDK, mapped with `-P android=<image>`. Test a Gradle `assembleDebug`. Emulator tests need KVM on the worker.
- [ ] `.selfaction/workflows/platforms.yml`: build a small sample app on all five and use it as the end-to-end test.

### 4. Secrets and artifacts for real builds

- [ ] Per-repository secrets on the server, encrypted at rest, passed to jobs as `secrets.*` and never shown on job pages.
- [ ] Send uploaded artifacts back to the server, link them from the job page, and expire them like caches.

## Other open items

- [ ] Dependabot alert #24 / PR #3 (`moby/go-archive` 0.3.x): it breaks act v0.2.89 (`archive.Uncompressed` was removed). act only creates tar archives, and the advisory is about extracting them, so the vulnerable path isn't used. Dismiss the alert as "vulnerable code not used", or upgrade when an act release supports go-archive 0.3.
- [ ] Add a `CLOUDFLARE_API_TOKEN` repository secret (Cloudflare's "Edit Cloudflare Workers" token template) so the `site` workflow deploys on every push to `main`.
- [ ] GitHub billing lock: jobs on GitHub-hosted runners fail until it's fixed; self-hosted and server-mode jobs are unaffected.
- [ ] The agent service on the MacBook (`dev.selfaction.agent`, label `selfaction-local`) keeps taking jobs while that machine is on.
