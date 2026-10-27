---
title: The 1.5 GB cache that wasn't ours to upload
description: A routine setup-go step took 18 minutes on a self-hosted runner. It was downloading a copy of the whole machine's Go module cache, which an earlier job had uploaded to a public repository.
date: 2026-10-27
---

A job on the self-hosted MacBook had been running for twenty minutes, most of it inside `actions/setup-go`. The step normally takes seconds. Its log showed what it was doing:

```txt
Found in cache @ …/slot-2/_work/_tool/go/1.26.0/arm64
/Users/<owner>/go/pkg/mod
Cache hit for: setup-go-macOS-arm64-go-1.26.0-e5b0ccefb0e2…
Received 0 of 1548700148 (0.0%), 0.0 MBs/sec
Received 12582912 of 1548700148 (0.8%), 1.7 MBs/sec
…
```

It was downloading 1.5 GB at about 1.5 MB a second. The step finished after 1,094 seconds.

## Whose cache was it?

The second line is the answer. `setup-go` asks Go where its module cache is, and Go said `/Users/<owner>/go/pkg/mod`: the cache of the person who owns the laptop. In host mode, selfaction's agent started GitHub's runner as that user with that user's `HOME`. So the job's "module cache" was every Go module that machine had ever downloaded, for every project on it.

An earlier job had already spent 164 seconds in setup-go's post step, uploading all of it to the repository's Actions cache. GitHub listed what had piled up:

```txt
1607MB  dependabot/go_modules/github.com/go-git/go-git/v5-5.19.2
1583MB  dependabot/go_modules/github.com/klauspost/compress-1.18.7
1510MB  dependabot/go_modules/github.com/moby/sys/user-0.4.1
1476MB  main
1507MB  dependabot/go_modules/golang.org/x/crypto-0.52.0
 402MB  main (Linux, from the Docker-mode runner)
```

That was eight gigabytes. Four of the entries came from Dependabot branches, because the workflow ran on a push to any branch. And the repository is public, so workflows in it, including fork pull requests once approved, can restore those caches.

We deleted every entry at once with `gh cache delete --all`.

## The fix: a home per slot

A job should not run with its owner's home directory, and on GitHub-hosted runners it never does. Each slot of the agent now gets its own persistent `_home`, and jobs run with `HOME` pointing at it. Go, npm, pip and most other tools derive their caches from `HOME`, so they now land in the slot:

```txt
GOMODCACHE='…/slot-1/_home/go/pkg/mod'
GOCACHE='…/slot-1/_home/Library/Caches/go-build'
```

One thing had to keep pointing at the real home. Docker's configuration holds the contexts and registry logins that make `docker` work at all, so the agent sets `DOCKER_CONFIG` to the owner's `~/.docker`. If you really want the old behaviour, `--share-home` brings it back.

Two workflow changes came with it. Self-hosted jobs now run only for pushes to `main`. And `setup-go` runs with `cache: false`, because a machine that keeps its module cache between jobs gains nothing from shipping it to GitHub and back.

## Before and after

| | Before | After |
| --- | --- | --- |
| setup-go | 21 s, up to 1,094 s | 1 s |
| setup-go post step | 164 s | 0 s |
| Whole job | 3m 38s | [58 s](https://github.com/broisnischal/selfaction/actions/runs/40436118928) |
| Actions caches in the repository | 8 GB | none |

## If you run self-hosted runners

Check what `HOME` your jobs see. If it is a person's home directory, every cache action in your workflows is caching that person's machine, and on a public repository those caches are readable by workflows in that repository.
