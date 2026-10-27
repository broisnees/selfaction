---
title: GitHub stopped running our jobs. A laptop didn't.
description: An account billing lock took GitHub's hosted runners away mid-project. Jobs sent to a selfaction agent kept passing, with one surprise about private repositories.
date: 2026-10-26
---

The first push of selfaction to GitHub produced no CI at all. The Actions tab showed one entry, named `BuildFailed`, that ended in `startup_failure` without a single job. GitHub's own Dependency Graph workflow, which nobody had written, failed the same way.

## Ruling out the workflow files

`startup_failure` usually means GitHub could not parse a workflow. So the files went through [actionlint](https://github.com/rhysd/actionlint) first. It found nothing. A hand-dispatched run of the workflow that only targets self-hosted runners, which cannot cost GitHub a minute, failed in the same way within seconds.

The account's public repositories had run Actions that same day. Every repository that failed was private.

## Making the repository public

The repository was meant to be public anyway, so we flipped it, required approval for outside contributors' workflow runs, and pushed again. Runs started this time, and the hosted job finally explained itself:

> The job was not started because your account is locked due to a billing issue.

The other workflow, the one that targets `runs-on: [self-hosted, selfaction-local]`, did not care. Two jobs went to two slots of a selfaction agent on a MacBook and ran side by side:

| Run | Runner | Result | Time |
| --- | --- | --- | --- |
| [40356880273](https://github.com/broisnees/selfaction/actions/runs/40356880273) | `macbook-pro-1-e9e696` | passed | 3m 17s |
| [40356894290](https://github.com/broisnees/selfaction/actions/runs/40356894290) | `macbook-pro-2-c41842` | passed | 3m 38s |
| [40356894448](https://github.com/broisnees/selfaction/actions/runs/40356894448) | GitHub-hosted | refused | 3s |

GitHub still scheduled the jobs, streamed their logs and recorded the results. Only the machine changed.

## What we learned

**A billing lock is not only about hosted minutes.** On a private repository, the lock stopped runs before any job existed, self-hosted jobs included. On a public repository, only jobs for GitHub's hosted runners were refused. If your CI matters, know which of those two situations you would be in.

**`startup_failure` with no jobs is worth a second look.** When actionlint is happy and even a self-hosted dispatch fails, the problem is the account, not the YAML. GitHub's message only appears once a job actually exists.

**Self-hosted runners are a fallback you can test today.** Starting one takes a single command:

```sh
selfaction agent start --repo you/app --labels my-mac
```

Add `[self-hosted, my-mac]` to one job and you have a second place for CI to run, one that does not depend on the state of a billing page.

The lock is still on as this is written. Every job that has run on [the repository](https://github.com/broisnees/selfaction/actions) since then ran on that MacBook or in a container next to it.
