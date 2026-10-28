package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// enqueue stores a run with one queued job that needs labels.
func enqueue(t *testing.T, s *Store, name string, labels ...string) *Job {
	t.Helper()
	j := &Job{Key: name, Name: name, Labels: labels, Status: Queued}
	r := &Run{Repo: "o/r", SHA: "a", Event: "push", Workflow: ".github/workflows/" + name + ".yml", Name: name, Payload: []byte("{}"), Source: []byte("jobs: {}")}
	if err := s.CreateRun(context.Background(), r, []*Job{j}); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestQueueMatchesLabelsAndHandsOutEachJobOnce(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	linux := enqueue(t, s, "ci", "ubuntu-latest")
	mac := enqueue(t, s, "mac", "macos-latest")

	got, err := s.Claim(ctx, "w1", []string{"ubuntu-latest", "self-hosted"})
	if err != nil || got == nil || got.ID != linux.ID || got.Status != Running || got.Repo != "o/r" || got.WorkflowName != "ci" {
		t.Fatalf("w1 claim = %+v, %v; want the linux job with its run's fields", got, err)
	}
	if again, _ := s.Claim(ctx, "w1", []string{"ubuntu-latest"}); again != nil {
		t.Fatalf("a Linux worker must not get the macOS job, got %s", again.Name)
	}
	if got, _ := s.Claim(ctx, "w2", []string{"MacOS-Latest"}); got == nil || got.ID != mac.ID {
		t.Fatalf("labels must match case-insensitively, got %+v", got)
	}

	// Cancelling reaches the worker through its heartbeat.
	if _, err := s.Cancel(ctx, mac.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Heartbeat(ctx, mac.ID, "w2"); st != Cancelled {
		t.Errorf("heartbeat after cancel = %q", st)
	}
	if _, err := s.Finish(ctx, linux.ID, "someone-else", Success, nil); err == nil {
		t.Error("a worker finished a job it does not hold")
	}
	j, err := s.Finish(ctx, linux.ID, "w1", Success, map[string]string{"version": "1.2"})
	if err != nil || j.Status != Success || j.FinishedAt.IsZero() || j.Outputs["version"] != "1.2" {
		t.Fatalf("finish = %+v, %v", j, err)
	}
}

func TestRunsNumberAndSaveOnlyChangesWaitingJobs(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	a, b := enqueue(t, s, "ci"), enqueue(t, s, "ci")
	ra, _ := s.GetRun(ctx, a.RunID)
	rb, _ := s.GetRun(ctx, b.RunID)
	if rb.Number <= ra.Number || ra.RunNumber != 1 || rb.RunNumber != 2 {
		t.Errorf("run ids %d, %d and numbers %d, %d", ra.Number, rb.Number, ra.RunNumber, rb.RunNumber)
	}

	w := &Job{RunID: ra.ID, Key: "later", Name: "later", Status: Waiting}
	if err := s.Save(ctx, nil, []*Job{w}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	w.Status = Queued
	if err := s.Save(ctx, []*Job{w}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, w.ID); got.Status != Cancelled {
		t.Errorf("a cancelled job was released: %s", got.Status)
	}
	jobs, _ := s.Jobs(ctx, ra.ID)
	if len(jobs) != 2 || jobs[0].ID != a.ID || jobs[1].ID != w.ID {
		t.Errorf("run jobs = %v", jobs)
	}
}

func TestConcurrentClaimsNeverShareAJob(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	for range 10 {
		enqueue(t, s, "ci")
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			j, err := s.Claim(ctx, "w", nil)
			if err != nil {
				t.Error(err)
			}
			if j != nil {
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != 10 {
		t.Fatalf("%d distinct jobs claimed, want 10", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("job %s claimed %d times", id, n)
		}
	}
}

func TestReapFailsJobsWhoseWorkerWentSilent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	j := enqueue(t, s, "ci")
	_, _ = s.Claim(ctx, "w", nil)
	_, _ = s.db.Exec(`UPDATE jobs SET heartbeat_at = ? WHERE id = ?`, time.Now().Add(-10*time.Minute).Unix(), j.ID)

	lost, err := s.Reap(ctx, 3*time.Minute)
	if err != nil || len(lost) != 1 || lost[0].Status != Errored {
		t.Fatalf("reap = %+v, %v", lost, err)
	}
	if got, _ := s.Get(ctx, j.ID); got.Status != Errored {
		t.Errorf("status after reap = %s", got.Status)
	}
}

func TestDeliveriesAndLogs(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	if seen, _ := s.SeenDelivery(ctx, "d1"); seen {
		t.Error("first delivery reported as seen")
	}
	if seen, _ := s.SeenDelivery(ctx, "d1"); !seen {
		t.Error("redelivery not detected")
	}
	if b, err := s.Log("nope"); err != nil || len(b) != 0 {
		t.Errorf("missing log = %q, %v", b, err)
	}
	_ = s.AppendLog("j", []byte("one\n"))
	_ = s.AppendLog("j", []byte("two\n"))
	if b, _ := s.Log("j"); string(b) != "one\ntwo\n" {
		t.Errorf("log = %q", b)
	}
}

func TestLegacyJobsTableIsKept(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY, payload BLOB NOT NULL); CREATE INDEX jobs_queue ON jobs(id); INSERT INTO jobs VALUES ('old', x'00')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open a first-release store: %v", err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs_v1`).Scan(&n); err != nil || n != 1 {
		t.Errorf("jobs_v1 has %d rows (%v)", n, err)
	}
	enqueue(t, s, "ci")
}

func TestSecretsAreSealedPerRepository(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	sealer, err := LoadSealer(filepath.Join(t.TempDir(), "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed := sealer.Seal("O/App", "TOKEN", []byte("hunter2"))
	if err := s.SetSecret(ctx, "O/App", "TOKEN", sealed); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Store: s, Sealer: sealer}
	got, err := srv.repoSecrets(ctx, "o/app")
	if err != nil || got["TOKEN"] != "hunter2" {
		t.Fatalf("secrets = %v, %v", got, err)
	}
	if _, err := sealer.Open("o/other", "TOKEN", sealed); err == nil {
		t.Error("a sealed value opened for another repository")
	}
	if ok, _ := s.DeleteSecret(ctx, "o/app", "TOKEN"); !ok {
		t.Error("delete found nothing")
	}
	for _, bad := range []string{"1X", "GITHUB_TOKEN", "a-b", ""} {
		if _, err := normalizeSecretName(bad); err == nil {
			t.Errorf("accepted secret name %q", bad)
		}
	}
	if n, _ := normalizeSecretName("npm_token"); n != "NPM_TOKEN" {
		t.Errorf("names are upper-cased: %q", n)
	}
}

func TestStalledRuns(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	busy := enqueue(t, s, "busy")
	idle := enqueue(t, s, "idle")
	for _, j := range []*Job{busy, idle} {
		w := &Job{RunID: j.RunID, Key: "later", Name: "later", Status: Waiting}
		if err := s.Save(ctx, nil, []*Job{w}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Finish(ctx, idle.ID, "", Success, nil); err == nil {
		t.Fatal("finished a job no worker holds")
	}
	if _, err := s.End(ctx, idle.ID, Success); err != nil {
		t.Fatal(err)
	}
	got, err := s.StalledRuns(ctx)
	if err != nil || len(got) != 1 || got[0] != idle.RunID {
		t.Errorf("stalled = %v, %v; want only the run with nothing queued", got, err)
	}
}
