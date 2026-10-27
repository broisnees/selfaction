---
title: The runner that wouldn't stop
description: Pressing Ctrl-C took 32 seconds and left GitHub's runner running behind our back. The fix was one environment variable, found by reading a shell script.
date: 2026-10-26
---

The agent passed its first real test: two jobs from GitHub ran on a MacBook and both went green. Then we pressed Ctrl-C to check the other half of its job, stopping cleanly.

It took 32 seconds. GitHub showed zero runners afterwards, which looked right. Then `pgrep` showed what was really left:

```txt
25271 /bin/bash …/slot-1/run-helper.sh
25276 …/slot-1/bin/Runner.Listener run
25822 /bin/bash …/slot-2/run-helper.sh
25827 …/slot-2/bin/Runner.Listener run
```

Both listeners were still running, orphaned, still holding connections to GitHub. The 32 seconds was the agent's 30-second grace period running out before it killed the process it had started.

## Who got the signal

The agent starts GitHub's runner through its `run.sh` and, on shutdown, sends that process an interrupt. Reading `run.sh` showed why nothing happened. By default it runs the helper in the foreground:

```bash
run() {
    while :; do
        "$DIR"/run-helper.sh $*
        ...
```

Bash puts off handling a signal while it waits for a foreground child, and the signal had been sent to `run.sh` alone, not to its children. So `Runner.Listener` never heard about it. When the grace period ran out, the agent killed `run.sh`, and its children were adopted by the system and kept running.

## The switch that was already there

Further down, the same script has a second mode:

```bash
runWithManualTrap() {
    set -m
    trap 'kill -INT -$PID' INT TERM
    while :; do
        "$DIR"/run-helper.sh $* &
        PID=$!
        wait $PID
        ...
```

It runs the helper in its own process group and forwards INT and TERM to that whole group. It is used only when `RUNNER_MANUALLY_TRAP_SIG` is set, and nothing sets that by default.

Docker mode had the same bug in a worse form. Inside a container `run.sh` is process 1, and the kernel does not deliver signals to process 1 unless it has a handler for them. Without the trap, the container would only stop when Docker gave up and killed it.

## The fix

```go
cmd.Env = append(env,
    "ACTIONS_RUNNER_INPUT_JITCONFIG="+jit.Config,
    "RUNNER_MANUALLY_TRAP_SIG=1")
```

The same variable is passed into Docker-mode containers. Two smaller changes came with it. The grace period went up to 45 seconds, so a runner in the middle of a job has time to cancel it and report back. And on start, the agent now stops any listeners still running in its slot directories, which covers the case where the agent itself was killed outright.

The same test afterwards:

```txt
agents still running: 0 (after 4s)
runners on GitHub: 0
runner containers: 0
listener processes: 0
```

From 32 seconds with two orphans to 4 seconds with none.

## The lesson

Our unit test passed throughout. It used a fake `run.sh` that exited by itself, so it never checked that a signal reached the real runner. The bug only showed up when we checked the process table after stopping the agent. When software wraps someone else's process tree, look at what is still running after you stop it.
