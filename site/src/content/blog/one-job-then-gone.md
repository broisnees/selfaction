---
title: 'One job, then gone: how selfaction registers runners'
description: Why the agent never configures a long-lived runner, and what it takes to run GitHub's runner safely on a machine you also use.
date: 2026-10-26
---

The usual way to add a self-hosted runner is to copy a registration token from the repository settings, run `./config.sh`, and leave the runner running for months. It works, and it leaves a few problems behind. The runner's credentials sit on disk indefinitely. One job's leftovers can affect the next. And the runner is tied to one machine's setup by hand.

selfaction's agent never configures a runner that way. Every runner it starts exists for exactly one job.

## Just-in-time registrations

GitHub's REST API can create a *just-in-time* runner: one call with a name, a runner group and labels returns an encoded configuration that is valid for a single job.

```txt
POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig
{ "name": "macbook-pro-1-190459", "runner_group_id": 1,
  "labels": ["self-hosted", "macOS", "ARM64", "selfaction-local"] }
```

Each slot of the agent repeats a loop around that call: register, start GitHub's runner, wait for it to finish its one job, wipe the slot, register again. The runner names in our logs, such as `macbook-pro-1-e9e696` and then `macbook-pro-1-b9b360`, are those registrations coming and going.

## Keeping the credential out of sight

GitHub's documentation passes the configuration on the command line, `./run.sh --jitconfig …`. Anyone on the machine who runs `ps` can then read it. The runner also accepts any of its arguments from an environment variable prefixed with `ACTIONS_RUNNER_INPUT_`, and, as its source shows, it deletes those variables from its own environment once read. So the agent starts the runner with `ACTIONS_RUNNER_INPUT_JITCONFIG` instead.

## One directory per slot

A just-in-time runner writes its credentials (`.runner`, `.credentials`, `.credentials_rsaparams`) into the directory it runs from. Two runners in one directory would overwrite each other's, so the agent unpacks the runner once per slot. Before unpacking, it checks the archive's SHA-256 against the value GitHub publishes in each release's notes.

After every job the agent deletes the workspace, the diagnostics and those credential files. It keeps two things on purpose, because they make the next job faster: the tool cache that `actions/setup-go` and friends fill, and the slot's own home directory, which [came later and for a good reason](/blog/the-cache-that-was-not-ours/).

## When the runner goes quiet

One morning a Docker-mode runner showed as offline on GitHub while its container had been up for nine hours. The laptop had slept, and the runner's long poll to GitHub never noticed that it was dead.

Now every two minutes the agent asks GitHub how each of its registrations looks. Two offline answers in a row and the runner is stopped, and the slot registers a fresh one. A single bad answer, or an API error, changes nothing, so a brief network blip does not restart a runner in the middle of a job.

## The result

Whatever a job leaves behind, the next job on that slot starts from a clean workspace with a registration no other job has used. Stop the agent and its registrations are deleted from GitHub. Lose power, and the next start stops any runner processes that were left behind before it registers new ones.
