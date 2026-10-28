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
- [x] Optional: GitHub App authentication.
  - Sign a JWT (RS256) with the app's private key and exchange it at `/app/installations/{id}/access_tokens`, using `installation.id` from the delivery.
  - That gives short-lived per-repository tokens instead of one personal token, and enables the Checks API.
  - Done in Step 4: `--app-id` / `--app-key`, check runs instead of statuses. See "Also done in this step" below.

## Step 4: build for every platform, from any computer

Goal: one push builds for Linux, macOS, Windows, Android and iOS, and the server, a worker or `selfaction run` works on a Linux, Mac or Windows machine. The OS a job needs decides which worker runs it. Apple's license only allows macOS on Apple hardware, so macOS and iOS jobs always land on a Mac worker. "From any computer" means triggering and watching them from anywhere while the server routes each job to a worker that can run it.

Where it stands (2026-10-27, after this step):

- Release archives for linux, darwin and windows on amd64 and arm64 are built by `scripts/release.sh` and published by `.github/workflows/release.yml` on a `v*` tag. No tag has been pushed yet, so there is no release and the install scripts have nothing to download until there is.
- Server mode queues every job of a workflow on its own (one per matrix combination), releases jobs as their `needs` finish, and a worker runs just that job. Each job has its own status (or check run).
- Jobs get repository secrets, and artifacts go through the server, so jobs on different workers share them.
- Workers serve `windows-*` on Windows, `macos-*` on a Mac, `ubuntu-*-arm` on ARM64, and `--no-default-labels` makes single-purpose workers (Android, emulated ARM).
- `agent install` and `worker install` create a scheduled task on Windows; agent host mode downloads the Windows runner. None of the Windows code has run on Windows yet.

### 1. Install and run on any computer

- [x] Release workflow: on a tag, build linux, darwin and windows for amd64 and arm64, and attach the archives and checksums to a GitHub release.
  - `scripts/release.sh <version> [dir]` builds static binaries (`CGO_ENABLED=0`), `.tar.gz` for Linux and macOS, `.zip` for Windows, and `checksums.txt`. Built and checked locally for darwin/arm64, linux/amd64, linux/arm64 and windows/amd64.
  - `.github/workflows/release.yml` runs it on `v*` tags and publishes with `gh release create --generate-notes`. It runs on the `selfaction-local` agent by default because GitHub-hosted runners are blocked by the billing lock; set the `RELEASE_RUNS_ON` repository variable (a JSON label list) to move it. The first real run happens with the first tag.
- [x] Install script for Linux and macOS, and a zip for Windows. Update the install guide.
  - `install.sh` (curl or wget, SHA-256 checked, `/usr/local/bin` or `~/.local/bin`): tested on macOS and in Alpine (busybox wget, linux/arm64) against a local copy of a release, including a tampered archive being refused.
  - `install.ps1` (`irm … | iex`, checksum checked, `%LOCALAPPDATA%\Programs\selfaction`, added to the user PATH): parses in PowerShell 7; not run on Windows.
  - Later: scoop or winget manifests once releases exist.
- [ ] Test `run`, `server`, `worker` and `cache` on Windows and fix what breaks: paths, stopping (Ctrl-C and service stop), Docker Desktop.
  - Done without a Windows machine: every package builds and vets for windows/amd64 and windows/arm64; Windows labels; `run.cmd` and the Windows runner `.zip` for agent host mode; no `pkill`; runners are killed when they can't be interrupted; a stop file (`--stop-file`) so a scheduled task stops gracefully; `--log-file` because tasks have no stdout.
  - Still needs a Windows machine: actually running all four commands, Docker Desktop paths, and act's host mode (below).
- [x] `agent install` and a worker service on Windows (a Windows service or a scheduled task).
  - New `internal/service`: launchd, `systemd --user`, and on Windows a Task Scheduler task (UTF-16 XML: logon trigger, no time limit, restart on failure). Shared by `agent install` and the new `worker install` / `worker uninstall`. Uninstall on Windows writes the stop file, waits up to 90 s, then ends the task. Definitions are unit-tested; the Windows path has not run on Windows.

### 2. Split server-mode jobs per workflow job

- [x] Queue each job of a workflow on its own, with its own `runs-on` labels, and release a job once everything in its `needs` has passed.
  - `internal/server/plan.go`: a run row stores the workflow file; job rows start `waiting`. After each change the run is re-planned under one lock: needs done → `if` evaluated with act's expression engine (`github` and `needs` contexts, default `success()`), then `queued`, `skipped` or `failure`. Unknown needs and cycles are refused like on GitHub. The reaper re-plans stalled runs (e.g. after a restart).
  - `needs.<job>.outputs`: the worker reports the job's outputs with its result; a claim carries the results and outputs of all transitive needs. `run.Options.Only` runs one job with act: the needed jobs get `Result` and `Outputs` set instead of running, which is what act's `success()`/`failure()` and `needs` context read.
- [x] Expand `strategy.matrix` into separate jobs before they are claimed.
  - Static matrices at plan time, matrices built from expressions (`fromJSON(needs.x.outputs.y)`) when their needs finish; `include`/`exclude` via act. `runs-on` and `name` per combination. `fail-fast` (default on) cancels sibling combinations; `max-parallel` throttles. An empty matrix vector fails the job, as on GitHub.
- [x] One commit status per job, such as `selfaction/build (macos-latest) (push)`.
  - Named like GitHub's checks: `selfaction/<workflow> / <job> (<matrix>) (<event>)`. Updates per job are sent in order and coalesced. Run pages at `/runs/{id}`; `server cancel` takes a run id too.
- Verified on 2026-10-27 with the real binary, act and actions (only GitHub's REST API faked): a workflow with `build` → a two-way matrix `test` on two workers (a container on one, the Mac host on the other) → `on-failure` (`if: failure()`, skipped) → `report` (`if: always()`). `needs.build.outputs.version` and the matrix job's output arrived, the artifact crossed workers, the secret was masked, and every job got its own status. Unit tests cover dynamic matrices, fail-fast, max-parallel, skipping, cancelled combinations and stalled runs.

### 3. Platforms

- [x] **Linux**: amd64 works in Docker. Test arm64 on an amd64 host with `--container-architecture linux/arm64` and QEMU (binfmt).
  - This machine is arm64, so the mirror case was tested: the same job reports `x86_64` with `--container-architecture linux/amd64` and `aarch64` without. On an amd64 host, `docker run --privileged --rm tonistiigi/binfmt --install arm64` registers the emulator (documented); not run here.
  - ARM64 workers serve `ubuntu-24.04-arm` and `ubuntu-22.04-arm` by default. `--no-default-labels` gives an emulating worker only its own labels.
- [x] **macOS**: a Mac worker runs `macos-*` jobs on the host. Check what host-mode jobs share between runs (HOME, tool caches); the agent gives each slot its own HOME. Look at Tart VMs on Apple silicon for jobs that need isolation.
  - Worker host jobs now get `HOME=<home>/worker/home/<slot>` (and `DOCKER_CONFIG` pointing at yours), like the agent; `--share-home` turns it off. Verified in the end-to-end run. Still shared: system-wide installs (Homebrew, Xcode), `/tmp`, simulators, act's action cache.
  - Tart: clones a macOS VM from an OCI image (`ghcr.io/cirruslabs/macos-*-xcode`) copy-on-write in seconds; jobs would run over SSH inside it; Apple's license allows two macOS VMs per Mac; Tart is under the Functional Source License 1.1 (internal use allowed, competing products not). Not implemented: it needs a worker mode that ships the job into the VM. Written up in the platforms guide; on the roadmap.
- [x] **iOS**: on a Mac worker with Xcode (`--labels xcode`). Start with an unsigned simulator build with `xcodebuild`, then a signed archive once secrets exist.
  - `examples/platforms/ios` (SwiftUI package) builds for the iOS simulator with Xcode 27.1 here, directly and as a server-mode job.
  - [ ] Signed archive: secrets now exist, but it needs your Apple certificate, provisioning profile and App Store Connect key. Steps are in the platforms guide.
- [ ] **Windows**: `windows-latest` and `windows-2022` labels that run on the host of a Windows worker. Check act's host mode on Windows with the `pwsh`, `powershell` and `cmd` shells.
  - Done: `windows-latest`, `windows-2025` and `windows-2022` map to the host on Windows workers. act's source defaults `run:` steps on a Windows host to `pwsh`, falling back to `powershell`.
  - Needs a Windows machine: running jobs with each shell.
- [x] **Android**: a Linux image with the Android SDK, mapped with `-P android=<image>`. Test a Gradle `assembleDebug`. Emulator tests need KVM on the worker.
  - `images/android/Dockerfile`: `catthehacker/ubuntu:act-24.04` + OpenJDK 17 + cmdline-tools 16111833 (SHA-1 checked) + platform 35, build-tools 35.0.0, platform-tools + Gradle 8.14.6 (SHA-256 checked). linux/amd64 only (the build tools are x86-64); 4.25 GB.
  - `examples/platforms/android` (AGP 8.11.2, plain Java, no AndroidX): `assembleDebug` produced `app-debug.apk` in 3m 6s under amd64 emulation on this Mac. The image is built locally, not published.
  - Emulator tests need an x86 Linux worker with `/dev/kvm`; not tried.
- [x] `.selfaction/workflows/platforms.yml`: build a small sample app on all five and use it as the end-to-end test.
  - Ran on 2026-10-27 through the real binary: a server and three single-purpose workers on this Mac (Linux containers; the Mac host with `--labels xcode`; the Android image under amd64 emulation), with GitHub's REST API faked and a local bare repository. `cli ubuntu-latest` passed in 32 s, `cli macos-latest` in 1m 0s, `ios` in 21 s (`BUILD SUCCEEDED`), `android` in 2m 48s (`app-debug.apk`), and `summary` (`if: always()`) downloaded all three artifacts. `cli windows-latest` waited in the queue, because no worker serves its label, until it was cancelled. It still needs a Windows worker to pass.
  - Not yet run against GitHub itself: that needs the server with a webhook, as in Step 3, and a pushed commit.

### 4. Secrets and artifacts for real builds

- [x] Per-repository secrets on the server, encrypted at rest, passed to jobs as `secrets.*` and never shown on job pages.
  - AES-256-GCM with the repo and name as associated data, key in `<home>/server/secrets.key` (0600). `selfaction server secret set|ls|rm`, API under `/api/v1/repos/{owner}/{repo}/secrets`. GitHub's name rules and 48 KB limit. Fork pull requests get none. act masks the values (verified: `***` in logs).
- [x] Send uploaded artifacts back to the server, link them from the job page, and expire them like caches.
  - The server implements the artifact protocol of `upload-artifact`/`download-artifact` v4+ (Twirp + Azure-style blob uploads, block lists in order, SHA-256 digest checked at finalize and returned for download-artifact's own check, `archive: false` single files with their content type). Each job's runtime token is HMAC-signed, scoped to its run and only valid while the job runs.
  - Listed and downloadable on the run page; `--artifact-retention` (90 days) and `retention-days` expire them; `--no-artifacts` falls back to per-worker storage.
  - Verified with the real actions: `upload-artifact@v7` in a container on one worker, `download-artifact@v8` on the Mac host on another, digests matching.

### Also done in this step

- GitHub App authentication (Step 3, item 5): `--app-id` / `--app-key`; RS256 app JWT, cached installation tokens per `installation.id`, check runs (created, then patched) instead of statuses. Unit-tested against a fake API with the signature verified; not yet run against a real GitHub App (one has to be created in GitHub's settings).

## Other open items

- [x] Dependabot alert #24 / PR #3 (`moby/go-archive` 0.3.x): dismissed on 2026-10-27 as "vulnerable code not used". act is still at v0.2.89 (latest), its only use is `archive.TarWithOptions` in `pkg/container/docker_build.go`, and no extraction routine is reachable. PR #3 is still open; close it, or merge it once an act release supports go-archive 0.3.
- [ ] Add a `CLOUDFLARE_API_TOKEN` repository secret (Cloudflare's "Edit Cloudflare Workers" token template) so the `site` workflow deploys on every push to `main`. Needs a token created in the Cloudflare dashboard: `gh secret set CLOUDFLARE_API_TOKEN`.
- [ ] GitHub billing lock: jobs on GitHub-hosted runners fail until it's fixed; self-hosted and server-mode jobs are unaffected. Fixed in GitHub's billing settings.
- [ ] The agent service on the MacBook (`dev.selfaction.agent`, label `selfaction-local`) keeps taking jobs while that machine is on. The release workflow uses it too.
