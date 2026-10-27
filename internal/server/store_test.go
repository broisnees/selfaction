package server

import (
	"context"
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

func TestQueueMatchesLabelsAndHandsOutEachJobOnce(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	linux := &Job{Repo: "o/r", SHA: "a", Workflow: "ci.yml", Name: "ci", Labels: []string{"ubuntu-latest"}, Payload: []byte("{}")}
	mac := &Job{Repo: "o/r", SHA: "a", Workflow: "mac.yml", Name: "mac", Labels: []string{"macos-latest"}, Payload: []byte("{}")}
	for _, j := range []*Job{linux, mac} {
		if err := s.Enqueue(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Claim(ctx, "w1", []string{"ubuntu-latest", "self-hosted"})
	if err != nil || got == nil || got.ID != linux.ID || got.Status != Running {
		t.Fatalf("w1 claim = %+v, %v; want the linux job", got, err)
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
	if _, err := s.Finish(ctx, linux.ID, "someone-else", Success); err == nil {
		t.Error("a worker finished a job it does not hold")
	}
	if j, err := s.Finish(ctx, linux.ID, "w1", Success); err != nil || j.Status != Success || j.FinishedAt.IsZero() {
		t.Fatalf("finish = %+v, %v", j, err)
	}
}

func TestConcurrentClaimsNeverShareAJob(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	for range 10 {
		if err := s.Enqueue(ctx, &Job{Repo: "o/r", Payload: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
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
	j := &Job{Repo: "o/r", Payload: []byte("{}")}
	_ = s.Enqueue(ctx, j)
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
