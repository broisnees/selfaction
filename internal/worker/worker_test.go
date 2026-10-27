package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"

	"github.com/broisnees/selfaction/internal/github"
	"github.com/broisnees/selfaction/internal/run"
	"github.com/broisnees/selfaction/internal/server"
)

// gitRepo makes a bare repository with one commit on main and returns its URL and the commit.
func gitRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bare, work := filepath.Join(root, "origin.git"), filepath.Join(root, "work")
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "init", "-q", "--bare", "-b", "main", bare)
	git(root, "init", "-q", "-b", "main", work)
	_ = os.WriteFile(filepath.Join(work, "hello.txt"), []byte("hi\n"), 0o644)
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "first")
	git(work, "push", "-q", bare, "main")
	return "file://" + bare, git(work, "rev-parse", "HEAD")
}

func TestWorkerRunsClaimedJobs(t *testing.T) {
	FlushEvery = 50 * time.Millisecond
	cloneURL, sha := gitRepo(t)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }))
	defer gh.Close()
	store, err := server.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv := &server.Server{Store: store, GitHub: &github.Client{API: gh.URL, Token: "gh-token"}, WorkerToken: "wt",
		Log: logrus.New(), PollFor: 200 * time.Millisecond}
	api := httptest.NewServer(srv.Handler())
	defer api.Close()

	enqueue := func(name string) *server.Job {
		j := &server.Job{Repo: "o/app", SHA: sha, Ref: "refs/heads/main", Event: "push", Workflow: ".github/workflows/" + name + ".yml",
			Name: name, Labels: []string{"ubuntu-latest"}, Payload: []byte(`{"after":"` + sha + `"}`), CloneURL: cloneURL}
		if err := store.Enqueue(t.Context(), j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	ok, broken, slow := enqueue("ok"), enqueue("broken"), enqueue("slow")

	started := make(chan struct{})
	logger, logs := test.NewNullLogger()
	w := &Worker{
		Server: api.URL, Token: "wt", Name: "box", Labels: []string{"ubuntu-latest", "self-hosted"}, Slots: 3,
		Dir: t.TempDir(), Log: logger,
		Execute: func(ctx context.Context, o run.Options) error {
			switch filepath.Base(o.Workflows) {
			case "ok.yml":
				branch, _ := exec.Command("git", "-C", o.Dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
				event, _ := os.ReadFile(o.EventPath)
				if _, err := os.Stat(filepath.Join(o.Dir, "hello.txt")); err != nil || strings.TrimSpace(string(branch)) != "main" ||
					string(event) != `{"after":"`+sha+`"}` || !slices.Contains(o.Secrets, "GITHUB_TOKEN=gh-token") {
					return fmt.Errorf("bad checkout or options: branch %q event %q secrets %v", branch, event, o.Secrets)
				}
				fmt.Fprintln(o.Log, "[ok/test]   | step output")
				return nil
			case "broken.yml":
				return errors.New("Job 'test' failed")
			default:
				close(started)
				<-ctx.Done() // runs until the server cancels it
				return ctx.Err()
			}
		},
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go w.Run(ctx)

	<-started
	if _, err := store.Cancel(t.Context(), slow.ID); err != nil {
		t.Fatal(err)
	}
	status := func(j *server.Job) string { got, _ := store.Get(t.Context(), j.ID); return got.Status }
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (status(ok) != server.Success || status(broken) != server.Failure) {
		time.Sleep(50 * time.Millisecond)
	}
	if status(ok) != server.Success || status(broken) != server.Failure || status(slow) != server.Cancelled {
		t.Fatalf("statuses: ok=%s broken=%s slow=%s", status(ok), status(broken), status(slow))
	}

	logOf := func(j *server.Job) string { b, _ := store.Log(j.ID); return string(b) }
	time.Sleep(200 * time.Millisecond) // the final flush follows the result closely
	if l := logOf(ok); !strings.Contains(l, "Fetching o/app at "+sha[:7]) || !strings.Contains(l, "step output") {
		t.Errorf("ok log:\n%s", l)
	}
	if l := logOf(broken); !strings.Contains(l, "Job 'test' failed") {
		t.Errorf("broken log does not explain the failure:\n%s", l)
	}
	entries, _ := os.ReadDir(filepath.Join(w.Dir, "jobs"))
	for range 50 {
		if entries, _ = os.ReadDir(filepath.Join(w.Dir, "jobs")); len(entries) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(entries) != 0 {
		t.Errorf("workspaces left behind: %v", entries)
	}

	// The server already ended the cancelled job, so the worker must not report it again.
	slowDone := func() bool {
		for _, e := range logs.AllEntries() {
			if strings.Contains(e.Message, "job "+slow.ID+" "+server.Cancelled) {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(5 * time.Second); !slowDone() && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if !slowDone() {
		t.Fatal("the worker never finished the cancelled job")
	}
	for _, e := range logs.AllEntries() {
		if e.Level <= logrus.WarnLevel {
			t.Errorf("worker logged %s: %s", e.Level, e.Message)
		}
	}
}
