// Package server runs workflows without GitHub's runner service: it takes
// GitHub webhooks, queues one job per triggered workflow, hands jobs to
// selfaction workers, and reports results back as commit statuses.
package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Job statuses. "error" means the job could not finish, e.g. its worker vanished.
const (
	Queued    = "queued"
	Running   = "running"
	Success   = "success"
	Failure   = "failure"
	Cancelled = "cancelled"
	Errored   = "error"
)

// Job is one workflow run for one event on one commit.
type Job struct {
	ID          string    `json:"id"` // random: it also names the public log URL
	Repo        string    `json:"repo"`
	SHA         string    `json:"sha"`
	Ref         string    `json:"ref"`
	Event       string    `json:"event"`
	Workflow    string    `json:"workflow"` // path, e.g. .github/workflows/ci.yml
	Name        string    `json:"name"`     // workflow name, used as the status context
	Labels      []string  `json:"labels"`   // runs-on labels a worker must serve
	Payload     []byte    `json:"-"`        // the webhook payload, the job's github.event
	CloneURL    string    `json:"clone_url"`
	Status      string    `json:"status"`
	Worker      string    `json:"worker,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
	HeartbeatAt time.Time `json:"-"`
}

// Done reports whether the job has reached a final status.
func (j *Job) Done() bool { return j.Status != Queued && j.Status != Running }

var ErrNotFound = errors.New("job not found")

// Store keeps the queue in SQLite and job logs as files.
type Store struct {
	db   *sql.DB
	logs string
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id           TEXT PRIMARY KEY,
	repo         TEXT NOT NULL,
	sha          TEXT NOT NULL,
	ref          TEXT NOT NULL,
	event        TEXT NOT NULL,
	workflow     TEXT NOT NULL,
	name         TEXT NOT NULL,
	labels       TEXT NOT NULL,
	payload      BLOB NOT NULL,
	clone_url    TEXT NOT NULL,
	status       TEXT NOT NULL,
	worker       TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	started_at   INTEGER NOT NULL DEFAULT 0,
	finished_at  INTEGER NOT NULL DEFAULT 0,
	heartbeat_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS jobs_queue ON jobs(status, created_at);
CREATE TABLE IF NOT EXISTS deliveries (
	id TEXT PRIMARY KEY,
	at INTEGER NOT NULL
);`

func Open(dir string) (*Store, error) {
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "server.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init server store: %w", err)
	}
	return &Store{db: db, logs: logs}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SeenDelivery records a webhook delivery id and reports whether it was
// already handled; GitHub redelivers on timeouts and on request.
func (s *Store) SeenDelivery(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO deliveries (id, at) VALUES (?, ?)`, id, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 0, err
}

func (s *Store) Enqueue(ctx context.Context, j *Job) error {
	j.ID, j.Status, j.CreatedAt = newID(), Queued, time.Now()
	labels, _ := json.Marshal(j.Labels)
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs (id, repo, sha, ref, event, workflow, name, labels, payload, clone_url, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Repo, j.SHA, j.Ref, j.Event, j.Workflow, j.Name, string(labels), j.Payload, j.CloneURL, j.Status, j.CreatedAt.Unix())
	return err
}

const jobCols = `id, repo, sha, ref, event, workflow, name, labels, payload, clone_url, status, worker, created_at, started_at, finished_at, heartbeat_at`

type scanner interface{ Scan(...any) error }

func scanJob(r scanner) (*Job, error) {
	var j Job
	var labels string
	var created, started, finished, beat int64
	if err := r.Scan(&j.ID, &j.Repo, &j.SHA, &j.Ref, &j.Event, &j.Workflow, &j.Name, &labels, &j.Payload, &j.CloneURL,
		&j.Status, &j.Worker, &created, &started, &finished, &beat); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(labels), &j.Labels)
	j.CreatedAt = time.Unix(created, 0)
	for t, v := range map[*time.Time]int64{&j.StartedAt: started, &j.FinishedAt: finished, &j.HeartbeatAt: beat} {
		if v > 0 {
			*t = time.Unix(v, 0)
		}
	}
	return &j, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// List returns the newest jobs first.
func (s *Store) List(ctx context.Context, limit int) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// serves reports whether a worker with these labels can take a job: like
// GitHub, it must carry every label the job asks for (case-insensitive).
func serves(worker, required []string) bool {
	for _, r := range required {
		if !slices.ContainsFunc(worker, func(w string) bool { return strings.EqualFold(w, r) }) {
			return false
		}
	}
	return true
}

// Claim hands the oldest queued job this worker can serve to it, or returns nil.
func (s *Store) Claim(ctx context.Context, worker string, labels []string) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// phobos: scans the 200 oldest queued jobs; index labels if queues grow far beyond that.
	rows, err := tx.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE status = ? ORDER BY created_at, rowid LIMIT 200`, Queued)
	if err != nil {
		return nil, err
	}
	var pick *Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if serves(labels, j.Labels) {
			pick = j
			break
		}
	}
	rows.Close()
	if pick == nil {
		return nil, nil
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ?, worker = ?, started_at = ?, heartbeat_at = ? WHERE id = ? AND status = ?`,
		Running, worker, now.Unix(), now.Unix(), pick.ID, Queued); err != nil {
		return nil, err
	}
	pick.Status, pick.Worker, pick.StartedAt, pick.HeartbeatAt = Running, worker, now, now
	return pick, tx.Commit()
}

// Heartbeat marks a running job as alive and returns its status, so a worker
// learns about a cancellation.
func (s *Store) Heartbeat(ctx context.Context, id, worker string) (string, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET heartbeat_at = ? WHERE id = ? AND worker = ? AND status = ?`,
		time.Now().Unix(), id, worker, Running); err != nil {
		return "", err
	}
	j, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return j.Status, nil
}

// Finish records the result a worker reports for its running job.
func (s *Store) Finish(ctx context.Context, id, worker, status string) (*Job, error) {
	if status != Success && status != Failure && status != Cancelled {
		return nil, fmt.Errorf("invalid result %q", status)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, finished_at = ? WHERE id = ? AND worker = ? AND status = ?`,
		status, time.Now().Unix(), id, worker, Running)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("job %s is not running on %s", id, worker)
	}
	return s.Get(ctx, id)
}

// Cancel stops a queued or running job. A running job's worker sees the new
// status on its next heartbeat.
func (s *Store) Cancel(ctx context.Context, id string) (*Job, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, finished_at = ? WHERE id = ? AND status IN (?, ?)`,
		Cancelled, time.Now().Unix(), id, Queued, Running); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Reap fails running jobs whose worker has not sent a heartbeat within stale,
// and returns them.
func (s *Store) Reap(ctx context.Context, stale time.Duration) ([]*Job, error) {
	cutoff := time.Now().Add(-stale).Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE status = ? AND heartbeat_at < ?`, Running, cutoff)
	if err != nil {
		return nil, err
	}
	var lost []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		lost = append(lost, j)
	}
	rows.Close()
	now := time.Now()
	for _, j := range lost {
		if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, finished_at = ? WHERE id = ? AND status = ?`,
			Errored, now.Unix(), j.ID, Running); err != nil {
			return nil, err
		}
		j.Status, j.FinishedAt = Errored, now
	}
	return lost, nil
}

func (s *Store) logPath(id string) string { return filepath.Join(s.logs, id+".log") }

// AppendLog adds output to a job's log.
func (s *Store) AppendLog(id string, b []byte) error {
	f, err := os.OpenFile(s.logPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Log returns a job's log so far ("" before the first line arrives).
func (s *Store) Log(id string) ([]byte, error) {
	b, err := os.ReadFile(s.logPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}
