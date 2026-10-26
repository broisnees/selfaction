// Package cache implements a self-hosted backend for actions/cache: the
// GitHub Actions cache HTTP API (v1, the one act and GHES speak) plus the
// eviction and pruning tools a long-lived shared cache needs.
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	ErrExists   = errors.New("cache entry already exists or is being created")
	ErrNotFound = errors.New("cache entry not found")
)

// Entry is one saved cache archive. Scope isolates caches per repository.
type Entry struct {
	ID        int64
	Scope     string
	Key       string
	Version   string
	Size      int64
	Complete  bool
	CreatedAt time.Time
	UsedAt    time.Time
}

// Store keeps entry metadata in SQLite (WAL, so the server and CLI can share it)
// and archives as plain files under dir/blobs.
type Store struct {
	db  *sql.DB
	dir string
}

const schema = `
CREATE TABLE IF NOT EXISTS caches (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	scope      TEXT    NOT NULL,
	key        TEXT    NOT NULL,
	version    TEXT    NOT NULL,
	size       INTEGER NOT NULL DEFAULT 0,
	complete   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	used_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS caches_identity ON caches(scope, version, key);
CREATE INDEX IF NOT EXISTS caches_used ON caches(used_at);`

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "index.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init cache index: %w", err)
	}
	return &Store{db: db, dir: dir}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) blob(id int64) string {
	return filepath.Join(s.dir, "blobs", strconv.FormatInt(id, 10))
}

var scopeRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)

// ScopeOf turns "owner/repo" into a URL-safe scope ("owner.repo"). Valid scopes pass through unchanged.
func ScopeOf(repo string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '/':
			return '.'
		case r < 128 && scopeRe.MatchString(string(r)):
			return r
		default:
			return '-'
		}
	}, repo)
}

func ValidScope(scope string) bool { return scopeRe.MatchString(scope) }

const cols = `id, scope, key, version, size, complete, created_at, used_at`

func scanEntries(rows *sql.Rows) ([]Entry, error) {
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var created, used int64
		if err := rows.Scan(&e.ID, &e.Scope, &e.Key, &e.Version, &e.Size, &e.Complete, &created, &used); err != nil {
			return nil, err
		}
		e.CreatedAt, e.UsedAt = time.Unix(created, 0), time.Unix(used, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Find resolves keys the way GitHub does: for each key in order, an exact match
// wins, otherwise the newest entry whose key starts with it.
func (s *Store) Find(ctx context.Context, scope string, keys []string, version string) (*Entry, error) {
	for _, k := range keys {
		if k == "" {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM caches
			WHERE scope = ? AND version = ? AND complete = 1 AND substr(key, 1, length(?)) = ?
			ORDER BY key = ? DESC, created_at DESC LIMIT 1`, scope, version, k, k, k)
		if err != nil {
			return nil, err
		}
		found, err := scanEntries(rows)
		if err != nil {
			return nil, err
		}
		if len(found) == 1 {
			return &found[0], nil
		}
	}
	return nil, nil
}

// Reserve starts a new upload. Entries are immutable: an existing or in-flight
// entry for the same scope/key/version yields ErrExists.
func (s *Store) Reserve(ctx context.Context, scope, key, version string) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `INSERT INTO caches (scope, key, version, created_at, used_at) VALUES (?, ?, ?, ?, ?)`,
		scope, key, version, now, now)
	var serr *sqlite.Error
	if errors.As(err, &serr) && serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return 0, ErrExists
	}
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) get(ctx context.Context, scope string, id int64) (*Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM caches WHERE id = ? AND scope = ?`, id, scope)
	if err != nil {
		return nil, err
	}
	found, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, ErrNotFound
	}
	return &found[0], nil
}

func (s *Store) touch(ctx context.Context, id int64) {
	_, _ = s.db.ExecContext(ctx, `UPDATE caches SET used_at = ? WHERE id = ?`, time.Now().Unix(), id)
}

// Write stores one uploaded chunk at offset. Chunks may arrive concurrently and out of order.
func (s *Store) Write(ctx context.Context, scope string, id, offset int64, r io.Reader) (int64, error) {
	e, err := s.get(ctx, scope, id)
	if err != nil {
		return 0, err
	}
	if e.Complete {
		return 0, fmt.Errorf("cache %d is already committed", id)
	}
	f, err := os.OpenFile(s.blob(id), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(io.NewOffsetWriter(f, offset), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	s.touch(ctx, id)
	return n, err
}

// Commit finalizes an upload. size is the client's expected archive size (0 = unknown).
func (s *Store) Commit(ctx context.Context, scope string, id, size int64) error {
	e, err := s.get(ctx, scope, id)
	if err != nil {
		return err
	}
	if e.Complete {
		return fmt.Errorf("cache %d is already committed", id)
	}
	st, err := os.Stat(s.blob(id))
	if err != nil {
		return fmt.Errorf("cache %d has no uploaded data: %w", id, err)
	}
	if size > 0 && st.Size() != size {
		return fmt.Errorf("cache %d: uploaded %d bytes, expected %d", id, st.Size(), size)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE caches SET complete = 1, size = ?, used_at = ? WHERE id = ?`,
		st.Size(), time.Now().Unix(), id)
	return err
}

// OpenBlob opens a committed archive for download and marks it used.
func (s *Store) OpenBlob(ctx context.Context, scope string, id int64) (*os.File, *Entry, error) {
	e, err := s.get(ctx, scope, id)
	if err != nil {
		return nil, nil, err
	}
	if !e.Complete {
		return nil, nil, ErrNotFound
	}
	f, err := os.Open(s.blob(id))
	if err != nil {
		return nil, nil, err
	}
	s.touch(ctx, id)
	return f, e, nil
}

// Filter selects entries for List and Prune. Zero values match everything.
type Filter struct {
	Scope     string
	KeyPrefix string
	UnusedFor time.Duration // only entries not used within this long
}

func (s *Store) List(ctx context.Context, f Filter) ([]Entry, error) {
	q := `SELECT ` + cols + ` FROM caches WHERE 1 = 1`
	var args []any
	if f.Scope != "" {
		q += ` AND scope = ?`
		args = append(args, f.Scope)
	}
	if f.KeyPrefix != "" {
		q += ` AND substr(key, 1, length(?)) = ?`
		args = append(args, f.KeyPrefix, f.KeyPrefix)
	}
	if f.UnusedFor > 0 {
		q += ` AND used_at < ?`
		args = append(args, time.Now().Add(-f.UnusedFor).Unix())
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY scope, used_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

// Delete removes entries and their archives. Downloads already in progress finish
// from the open file handle.
func (s *Store) Delete(ctx context.Context, ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM caches WHERE id IN (?`+strings.Repeat(`, ?`, len(ids)-1)+`)`, args...); err != nil {
		return err
	}
	for _, id := range ids {
		if err := os.Remove(s.blob(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Prune deletes every entry matching f and returns what it removed.
func (s *Store) Prune(ctx context.Context, f Filter) ([]Entry, error) {
	entries, err := s.List(ctx, f)
	if err != nil {
		return nil, err
	}
	return entries, s.Delete(ctx, ids(entries)...)
}

func ids(entries []Entry) []int64 {
	out := make([]int64, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}
