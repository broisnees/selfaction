---
title: Server mode's first real push
description: selfaction's server and worker ran this repository's CI from GitHub webhooks through a quick tunnel. The job passed, a cancel took 4 seconds, two worker bugs came out on the way, and the shared cache turned out slower than no cache.
date: 2026-10-27
---

Server mode takes GitHub Actions out of the loop. GitHub sends a webhook, `selfaction server` queues the workflows it triggers, and `selfaction worker` runs them with the act engine and reports back as commit statuses. Until today it had only run against fakes: an `httptest` GitHub in the unit tests, then a small Python stand-in that served files from a local bare repository. This was the first run against GitHub itself.

## The setup

One Linux machine ran everything: the server, a worker with two slots, and a Cloudflare quick tunnel so GitHub could reach port 8080. The workflow went into `.selfaction/workflows/ci.yml` rather than `.github/workflows`, so GitHub Actions would not run it a second time.

```sh
selfaction server --external-url https://<random>.trycloudflare.com --workflows .selfaction/workflows
selfaction worker --server http://localhost:8080 --slots 2
```

The workflow runs `go vet`, `go test` and `go build`, with `actions/cache` keeping `~/go/pkg/mod`.

## A tunnel that answered 404

The first tunnel registered with Cloudflare and then answered every request with a 404. Nothing reached the server. Its settings line gave it away: it listed the credentials file of a named tunnel we run for something else, picked up from `~/.cloudflared/config.yml`. A quick tunnel started with an empty config file answered on the first try:

```sh
cloudflared --config empty.yml tunnel --url http://localhost:8080
```

We added the repository webhook, and GitHub's signed `ping` came back 200.

## The first push

Commit `0e50436` went from Queued to Running to Passed in 1m 48s, and its status linked to the job's page on the server, through the tunnel. Nothing was cached yet. `go vet` took 38.8 seconds, module downloads included, and saving the 312 MB module cache to the server took another 30.8.

## Bug one: a cancel that made noise

This one was known before the run. It just never failed anything. When a job is cancelled on the server, the worker finds out from the answer to its next log upload and stops the job. Then it posted the result to `/finish` anyway. The job was already final, so the server answered 409 and the worker logged an error. `TestWorkerRunsClaimedJobs` printed that line on every run, and passed.

The worker now skips `/finish` when the server has already ended the job. The test also fails on any warning the worker logs, so the next harmless-looking error line breaks the build instead of scrolling past.

On the second push, `ebd3875`, we cancelled the job with `selfaction server cancel` right after its cache restore. The worker noticed 2 seconds later, the job was gone 4 seconds after the API call, the commit status went to Cancelled, and the worker log had no warnings at all.

## Bug two: Ctrl-C on the worker

The second bug turned up while writing the docs, not during the run. The guide was going to say that stopping a worker with Ctrl-C cancels its running jobs and reports them. Reading the code to check that sentence showed it wasn't true:

```go
if res, err := w.post(ctx, "/api/v1/jobs/"+j.ID+"/finish", "application/json", body); err != nil {
```

`ctx` is the worker's own context, and Ctrl-C has just cancelled it, so the request fails before it is sent. The job would stay running on the server until the reaper, which fails jobs that go 3 minutes without a heartbeat, marked it "The worker stopped responding". The final log upload a few lines above already used `context.WithoutCancel`. The result didn't.

It now gets its own 30-second deadline. The new test failed first, with `a job whose worker stopped is running, want cancelled`, and then passed.

Both bugs sat in the same ten lines, the ones that report a result. The unit test went through the first on every run and never looked. Nothing went through the second.

## The cache that cost time

The third push, `f8dd8ad`, restored the cache and passed in 1m 30s. That's 18 seconds faster than the first, and the cache isn't why:

| Step | First push, no cache | Third push, cache hit |
| --- | --- | --- |
| `actions/setup-go` | 9.0s | 0.7s |
| `actions/cache` restore | 1.2s | 46.9s |
| `go vet` | 38.8s | 25.6s |
| `actions/cache` save | 30.8s | 0.1s |

setup-go got faster because act keeps Go in its `act-toolcache` volume between jobs. Neither run had a build cache, and the two `go vet` runs compiled nearly the same code, so their difference, about 13 seconds, is roughly what downloading the modules cost. Restoring those modules from the cache took 46.9.

The tunnel is the reason. Job containers fetch the cache from `--external-url`, so 312 MB left the machine for Cloudflare and came straight back to it. The second push's restore took 51 seconds the same way.

What the server needs is a cache address for workers, separate from the public one that GitHub and the status links use. Until it has one, `--no-cache` behind a tunnel keeps each worker's cache on its own disk, and the modules never leave the machine.
