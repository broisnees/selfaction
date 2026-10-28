// Package server runs workflows without GitHub's runner service: it takes
// GitHub webhooks, queues every job of each triggered workflow (one per matrix
// combination), releases jobs as their needs finish, hands them to selfaction
// workers, and reports results back as commit statuses or check runs.
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

// Job statuses. "waiting" jobs wait for their needs; "error" means the job
// could not finish, e.g. its worker vanished.
const (
	Waiting   = "waiting"
	Queued    = "queued"
	Running   = "running"
	Success   = "success"
	Failure   = "failure"
	Cancelled = "cancelled"
	Skipped   = "skipped"
	Errored   = "error"
)

// Final reports whether status is a final job status.
func Final(status string) bool {
	switch status {
	case Success, Failure, Cancelled, Skipped, Errored:
		return true
	}
	return false
}

// Run is one workflow triggered by one event on one commit.
type Run struct {
	ID           string    `json:"id"`     // random: it also names the public run page
	Number       int64     `json:"number"` // GITHUB_RUN_ID
	RunNumber    int64     `json:"run_number"`
	Repo         string    `json:"repo"`
	SHA          string    `json:"sha"`
	Ref          string    `json:"ref"`
	Event        string    `json:"event"`
	Workflow     string    `json:"workflow"` // path, e.g. .github/workflows/ci.yml
	Name         string    `json:"name"`     // workflow name
	Payload      []byte    `json:"-"`        // the webhook payload, the jobs' github.event
	Source       []byte    `json:"-"`        // the workflow file at SHA
	CloneURL     string    `json:"clone_url"`
	Fork         bool      `json:"fork"`         // a pull request from a fork: its jobs get no secrets
	Installation int64     `json:"installation"` // GitHub App installation id, 0 with a token
	CreatedAt    time.Time `json:"created_at"`
}

// Job is one job of a run, for one matrix combination.
type Job struct {
	ID          string            `json:"id"` // random: it also names the public log URL
	RunID       string            `json:"run_id"`
	Key         string            `json:"key"`  // the job's id in the workflow
	Name        string            `json:"name"` // display name, e.g. "build (macos-latest)"
	Matrix      map[string]any    `json:"matrix,omitempty"`
	Expand      bool              `json:"-"`      // a placeholder whose matrix is expanded once its needs finish
	Labels      []string          `json:"labels"` // runs-on labels a worker must serve, in runs-on order
	Status      string            `json:"status"`
	Outputs     map[string]string `json:"outputs,omitempty"`
	Worker      string            `json:"worker,omitempty"`
	CheckRun    int64             `json:"-"`
	CreatedAt   time.Time         `json:"created_at"`
	StartedAt   time.Time         `json:"started_at,omitzero"`
	FinishedAt  time.Time         `json:"finished_at,omitzero"`
	HeartbeatAt time.Time         `json:"-"`

	// From the run.
	Repo         string `json:"repo"`
	SHA          string `json:"sha"`
	Ref          string `json:"ref"`
	Event        string `json:"event"`
	Workflow     string `json:"workflow"`
	WorkflowName string `json:"workflow_name"`
	Installation int64  `json:"-"`
}

// Done reports whether the job has reached a final status.
func (j *Job) Done() bool { return Final(j.Status) }

var ErrNotFound = errors.New("not found")

// Store keeps runs, the job queue, secrets and artifact records in SQLite,
// and job logs as files.
type Store struct {
	db   *sql.DB
	logs string
}

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	number       INTEGER PRIMARY KEY AUTOINCREMENT,
	id           TEXT NOT NULL UNIQUE,
	run_number   INTEGER NOT NULL,
	repo         TEXT NOT NULL,
	sha          TEXT NOT NULL,
	ref          TEXT NOT NULL,
	event        TEXT NOT NULL,
	workflow     TEXT NOT NULL,
	name         TEXT NOT NULL,
	payload      BLOB NOT NULL,
	source       BLOB NOT NULL,
	clone_url    TEXT NOT NULL,
	fork         INTEGER NOT NULL DEFAULT 0,
	installation INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS jobs (
	id           TEXT PRIMARY KEY,
	run_id       TEXT NOT NULL,
	key          TEXT NOT NULL,
	name         TEXT NOT NULL,
	matrix       TEXT NOT NULL DEFAULT '',
	expand       INTEGER NOT NULL DEFAULT 0,
	labels       TEXT NOT NULL DEFAULT '[]',
	status       TEXT NOT NULL,
	outputs      TEXT NOT NULL DEFAULT '',
	worker       TEXT NOT NULL DEFAULT '',
	check_run    INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	started_at   INTEGER NOT NULL DEFAULT 0,
	finished_at  INTEGER NOT NULL DEFAULT 0,
	heartbeat_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS jobs_queue ON jobs(status, created_at);
CREATE INDEX IF NOT EXISTS jobs_run ON jobs(run_id);
CREATE TABLE IF NOT EXISTS deliveries (
	id TEXT PRIMARY KEY,
	at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS secrets (
	repo       TEXT NOT NULL,
	name       TEXT NOT NULL,
	value      BLOB NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (repo, name)
);
CREATE TABLE IF NOT EXISTS artifacts (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id     TEXT NOT NULL,
	job_id     TEXT NOT NULL,
	name       TEXT NOT NULL,
	size       INTEGER NOT NULL DEFAULT 0,
	digest     TEXT NOT NULL DEFAULT '',
	mime       TEXT NOT NULL DEFAULT '',
	ready      INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS artifacts_run ON artifacts(run_id, name);`

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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init server store: %w", err)
	}
	return &Store{db: db, logs: logs}, nil
}

// migrate creates the schema. The first server release queued whole
// workflows in a jobs table without runs; that table is kept as jobs_v1.
func migrate(db *sql.DB) error {
	var legacy int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name = 'payload'`).Scan(&legacy); err != nil {
		return err
	}
	if legacy > 0 {
		if _, err := db.Exec(`DROP INDEX IF EXISTS jobs_queue; ALTER TABLE jobs RENAME TO jobs_v1`); err != nil {
			return err
		}
	}
	_, err := db.Exec(schema)
	return err
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

// CreateRun stores a run and its first jobs, and fills in their ids, numbers and timestamps.
func (s *Store) CreateRun(ctx context.Context, r *Run, jobs []*Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r.ID, r.CreatedAt = newID(), time.Now()
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) + 1 FROM runs WHERE repo = ? AND workflow = ?`, r.Repo, r.Workflow).Scan(&r.RunNumber); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO runs (id, run_number, repo, sha, ref, event, workflow, name, payload, source, clone_url, fork, installation, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.RunNumber, r.Repo, r.SHA, r.Ref, r.Event, r.Workflow, r.Name, r.Payload, r.Source, r.CloneURL, r.Fork, r.Installation, r.CreatedAt.Unix())
	if err != nil {
		return err
	}
	if r.Number, err = res.LastInsertId(); err != nil {
		return err
	}
	for _, j := range jobs {
		j.RunID = r.ID
		j.fromRun(r)
		if err := insertJob(ctx, tx, j); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (j *Job) fromRun(r *Run) {
	j.Repo, j.SHA, j.Ref, j.Event, j.Workflow, j.WorkflowName, j.Installation = r.Repo, r.SHA, r.Ref, r.Event, r.Workflow, r.Name, r.Installation
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertJob(ctx context.Context, tx execer, j *Job) error {
	j.ID, j.CreatedAt = newID(), time.Now()
	_, err := tx.ExecContext(ctx, `INSERT INTO jobs (id, run_id, key, name, matrix, expand, labels, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.RunID, j.Key, j.Name, jsonText(j.Matrix), j.Expand, jsonList(j.Labels), j.Status, j.CreatedAt.Unix())
	return err
}

func jsonText[T any](v map[string]T) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

const runCols = `number, id, run_number, repo, sha, ref, event, workflow, name, payload, source, clone_url, fork, installation, created_at`

func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	var r Run
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id).Scan(&r.Number, &r.ID, &r.RunNumber, &r.Repo, &r.SHA,
		&r.Ref, &r.Event, &r.Workflow, &r.Name, &r.Payload, &r.Source, &r.CloneURL, &r.Fork, &r.Installation, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	r.CreatedAt = time.Unix(created, 0)
	return &r, err
}

const jobCols = `j.id, j.run_id, j.key, j.name, j.matrix, j.expand, j.labels, j.status, j.outputs, j.worker, j.check_run,
	j.created_at, j.started_at, j.finished_at, j.heartbeat_at, r.repo, r.sha, r.ref, r.event, r.workflow, r.name, r.installation`

const jobFrom = ` FROM jobs j JOIN runs r ON r.id = j.run_id`

type scanner interface{ Scan(...any) error }

func scanJob(sc scanner) (*Job, error) {
	var j Job
	var matrix, labels, outputs string
	var created, started, finished, beat int64
	if err := sc.Scan(&j.ID, &j.RunID, &j.Key, &j.Name, &matrix, &j.Expand, &labels, &j.Status, &outputs, &j.Worker, &j.CheckRun,
		&created, &started, &finished, &beat, &j.Repo, &j.SHA, &j.Ref, &j.Event, &j.Workflow, &j.WorkflowName, &j.Installation); err != nil {
		return nil, err
	}
	if matrix != "" {
		_ = json.Unmarshal([]byte(matrix), &j.Matrix)
	}
	if outputs != "" {
		_ = json.Unmarshal([]byte(outputs), &j.Outputs)
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

func (s *Store) queryJobs(ctx context.Context, where string, args ...any) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+jobFrom+` `+where, args...)
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

func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	jobs, err := s.queryJobs(ctx, `WHERE j.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, ErrNotFound
	}
	return jobs[0], nil
}

// Jobs returns a run's jobs in the order they were created.
func (s *Store) Jobs(ctx context.Context, runID string) ([]*Job, error) {
	return s.queryJobs(ctx, `WHERE j.run_id = ? ORDER BY j.rowid`, runID)
}

// List returns the newest jobs first.
func (s *Store) List(ctx context.Context, limit int) ([]*Job, error) {
	return s.queryJobs(ctx, `ORDER BY j.created_at DESC, j.rowid DESC LIMIT ?`, limit)
}

// Save writes the jobs a run's progress changed (status, name, matrix,
// labels) and adds new ones, in one transaction. A job is only changed while
// it is still waiting, so a concurrent cancel wins.
func (s *Store) Save(ctx context.Context, changed, added []*Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	for _, j := range changed {
		var finished int64
		if Final(j.Status) {
			j.FinishedAt = now
			finished = now.Unix()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ?, name = ?, matrix = ?, expand = ?, labels = ?, finished_at = ? WHERE id = ? AND status = ?`,
			j.Status, j.Name, jsonText(j.Matrix), j.Expand, jsonList(j.Labels), finished, j.ID, Waiting); err != nil {
			return err
		}
	}
	for _, j := range added {
		if err := insertJob(ctx, tx, j); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	rows, err := tx.QueryContext(ctx, `SELECT `+jobCols+jobFrom+` WHERE j.status = ? ORDER BY j.created_at, j.rowid LIMIT 200`, Queued)
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
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return status, err
}

// Finish records the result and outputs a worker reports for its running job.
func (s *Store) Finish(ctx context.Context, id, worker, status string, outputs map[string]string) (*Job, error) {
	if status != Success && status != Failure && status != Cancelled {
		return nil, fmt.Errorf("invalid result %q", status)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, outputs = ?, finished_at = ? WHERE id = ? AND worker = ? AND status = ?`,
		status, jsonText(outputs), time.Now().Unix(), id, worker, Running)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("job %s is not running on %s", id, worker)
	}
	return s.Get(ctx, id)
}

// Cancel stops a waiting, queued or running job. A running job's worker sees
// the new status on its next heartbeat. It returns the job as it is now.
func (s *Store) Cancel(ctx context.Context, id string) (*Job, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, finished_at = ? WHERE id = ? AND status IN (?, ?, ?)`,
		Cancelled, time.Now().Unix(), id, Waiting, Queued, Running); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// End gives a job that has not finished a final status, e.g. "error" when
// the server could not hand it to its worker.
func (s *Store) End(ctx context.Context, id, status string) (*Job, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, finished_at = ? WHERE id = ? AND status IN (?, ?, ?)`,
		status, time.Now().Unix(), id, Waiting, Queued, Running); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Reap ends running jobs whose worker has not sent a heartbeat within stale
// with status "error", and returns them.
func (s *Store) Reap(ctx context.Context, stale time.Duration) ([]*Job, error) {
	lost, err := s.queryJobs(ctx, `WHERE j.status = ? AND j.heartbeat_at < ?`, Running, time.Now().Add(-stale).Unix())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []*Job
	for _, j := range lost {
		res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, finished_at = ? WHERE id = ? AND status = ?`, Errored, now.Unix(), j.ID, Running)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			j.Status, j.FinishedAt = Errored, now
			out = append(out, j)
		}
	}
	return out, nil
}

// StalledRuns lists runs with waiting jobs but none queued or running:
// their next jobs can be decided now.
func (s *Store) StalledRuns(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT run_id FROM jobs WHERE status = ?
		AND run_id NOT IN (SELECT run_id FROM jobs WHERE status IN (?, ?))`, Waiting, Queued, Running)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetCheckRun remembers the GitHub check run that shows a job.
func (s *Store) SetCheckRun(ctx context.Context, id string, checkRun int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET check_run = ? WHERE id = ?`, checkRun, id)
	return err
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
