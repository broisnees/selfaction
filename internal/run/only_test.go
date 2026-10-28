package run

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// lockedBuffer collects job output from act's concurrent loggers.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

const onlyWorkflow = `name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    outputs:
      version: ${{ steps.v.outputs.version }}
    steps:
      - id: v
        run: echo "version=1.2.3" >> "$GITHUB_OUTPUT"
  test:
    needs: build
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [linux, mac]
    outputs:
      seen: ${{ steps.s.outputs.seen }}
    steps:
      - id: s
        run: echo "seen=${{ needs.build.outputs.version }}-${{ matrix.os }}" >> "$GITHUB_OUTPUT"
      - run: echo "the token is ${{ secrets.TOKEN }}, run $GITHUB_RUN_ID"
  report:
    needs: test
    if: failure()
    runs-on: ubuntu-latest
    steps:
      - run: echo "test was ${{ needs.test.result }}"
`

// onlyRepo writes the workflow into a fresh git repository.
func onlyRepo(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the workflow uses bash")
	}
	if raceEnabled {
		t.Skip("act's host environment has a data race of its own; run without -race")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), []byte(onlyWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func onlyOptions(dir, job string, log *lockedBuffer) Options {
	return Options{
		Dir: dir, Workflows: ".github/workflows/ci.yml", Event: "push", Job: job, Only: true,
		Platforms: []string{"ubuntu-latest=-self-hosted"}, NoCache: true, Log: log,
		Secrets: []string{"GITHUB_TOKEN=gh-test-token", "TOKEN=hunter2"}, Env: []string{"GITHUB_RUN_ID=42"},
		Outputs: map[string]string{},
	}
}

func TestOnlyRunsOneJobWithItsNeeds(t *testing.T) {
	dir := onlyRepo(t)
	var log lockedBuffer
	o := onlyOptions(dir, "test", &log)
	o.Combination = map[string]any{"os": "mac"}
	o.Needs = map[string]Need{"build": {Result: "success", Outputs: map[string]string{"version": "1.2.3"}}}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	if got := o.Outputs["seen"]; got != "1.2.3-mac" {
		t.Errorf("outputs = %v, want seen=1.2.3-mac\n%s", o.Outputs, log.String())
	}
	out := log.String()
	if strings.Contains(out, "version=1.2.3\" >>") || strings.Count(out, "Run Main") > 2 {
		t.Errorf("the build job ran, or more than one combination did:\n%s", out)
	}
	if strings.Contains(out, "hunter2") || !strings.Contains(out, "the token is ***, run 42") {
		t.Errorf("the secret was not masked, or the run id not passed:\n%s", out)
	}
}

func TestOnlyFollowsNeedsResults(t *testing.T) {
	dir := onlyRepo(t)

	// A failed need skips a job with the default if: success().
	var log lockedBuffer
	o := onlyOptions(dir, "test", &log)
	o.Combination = map[string]any{"os": "linux"}
	o.Needs = map[string]Need{"build": {Result: "failure"}}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	if strings.Contains(log.String(), "Run Main") {
		t.Errorf("test ran although build failed:\n%s", log.String())
	}

	// if: failure() runs it, and needs.<job>.result is the given result.
	var log2 lockedBuffer
	o = onlyOptions(dir, "report", &log2)
	o.Needs = map[string]Need{"build": {Result: "success"}, "test": {Result: "failure"}}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("run: %v\n%s", err, log2.String())
	}
	if !strings.Contains(log2.String(), "test was failure") {
		t.Errorf("report did not run with the failed need:\n%s", log2.String())
	}
}
