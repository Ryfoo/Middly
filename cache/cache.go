// Package cache is the SQLite-backed response store. WAL mode and a small
// connection pool give us concurrent-safe access without global locks at the
// Go layer; SQLite's row-level locking handles the rest.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Entry is one cached upstream response.
type Entry struct {
	Hash      string
	Status    int
	Headers   http.Header
	Body      []byte
	CreatedAt time.Time
}

// Record is a row from the cache used by the dashboard listing.
type Record struct {
	Hash      string
	Namespace string
	Method    string
	Path      string
	Status    int
	Size      int
	CreatedAt time.Time
}

// Store wraps the SQLite database. Safe for concurrent use.
type Store struct {
	db     *sql.DB
	ttl    time.Duration
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Open initialises the cache file and applies the schema. ttl == 0 disables expiry.
func Open(path string, ttl time.Duration) (*Store, error) {
	// Added mmap_size for significantly faster blob reads.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=mmap_size=268435456",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite is single-writer; allow a handful of readers via the pool.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{
		db:     db,
		ttl:    ttl,
		ctx:    ctx,
		cancel: cancel,
	}

	// Start the background janitor to handle expiries safely
	if ttl > 0 {
		s.wg.Add(1)
		go s.janitor()
	}

	return s, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS cache (
    hash       TEXT PRIMARY KEY,
    namespace  TEXT NOT NULL,
    method     TEXT NOT NULL,
    path       TEXT NOT NULL,
    status     INTEGER NOT NULL,
    headers    TEXT NOT NULL,
    body       BLOB,
    created_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cache_created   ON cache(created_at);
CREATE INDEX IF NOT EXISTS idx_cache_namespace ON cache(namespace);
`

// janitor runs in the background and sweeps expired records.
// This replaces deleteAsync, preventing goroutine exhaustion under heavy load.
func (s *Store) janitor() {
	defer s.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().UTC().Add(-s.ttl).Format(time.RFC3339Nano)
			_, _ = s.db.ExecContext(s.ctx, `DELETE FROM cache WHERE created_at < ?`, cutoff)
		}
	}
}

// Close gracefully stops the janitor, checkpoints the WAL, and releases the DB.
func (s *Store) Close() error {
	s.cancel()
	s.wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = s.Checkpoint(ctx)
	return s.db.Close()
}

// Checkpoint runs `PRAGMA wal_checkpoint(TRUNCATE)`.
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Get returns the entry for hash, or (nil, nil) if missing or expired.
func (s *Store) Get(ctx context.Context, hash string) (*Entry, error) {
	var (
		e          Entry
		headersStr string
	)

	query := `SELECT hash, status, headers, body, created_at FROM cache WHERE hash = ?`
	args := []any{hash}

	// Ensure we don't return stale data even if the janitor hasn't swept yet
	if s.ttl > 0 {
		query += ` AND created_at >= ?`
		args = append(args, time.Now().UTC().Add(-s.ttl).Format(time.RFC3339Nano))
	}

	row := s.db.QueryRowContext(ctx, query, args...)
	if err := row.Scan(&e.Hash, &e.Status, &headersStr, &e.Body, &e.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	if err := json.Unmarshal([]byte(headersStr), &e.Headers); err != nil {
		return nil, err
	}
	return &e, nil
}

// Put inserts or replaces the cached response.
func (s *Store) Put(ctx context.Context, namespace, method, path string, e *Entry) error {
	headers, err := json.Marshal(e.Headers)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO cache (hash, namespace, method, path, status, headers, body, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Hash, namespace, method, path, e.Status, string(headers), e.Body, now,
	)
	return err
}

// Clear removes every cached entry.
func (s *Store) Clear(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cache`)
	return err
}

// Count returns the number of stored entries.
func (s *Store) Count(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache`).Scan(&n)
	return n, err
}

// Recent returns the n most recently stored records (without bodies).
func (s *Store) Recent(ctx context.Context, n int) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT hash, namespace, method, path, status, COALESCE(LENGTH(body),0), created_at
         FROM cache ORDER BY created_at DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Hash, &r.Namespace, &r.Method, &r.Path, &r.Status, &r.Size, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Export writes every row as a JSON-lines stream to w.
func (s *Store) Export(ctx context.Context) ([]ExportRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT hash, namespace, method, path, status, headers, body, created_at FROM cache`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportRow
	for rows.Next() {
		var r ExportRow
		if err := rows.Scan(&r.Hash, &r.Namespace, &r.Method, &r.Path, &r.Status, &r.Headers, &r.Body, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExportRow is the JSON-friendly shape used by Export/Import.
type ExportRow struct {
	Hash      string    `json:"hash"`
	Namespace string    `json:"namespace"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Headers   string    `json:"headers"`
	Body      []byte    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Import upserts every row via a streaming channel to avoid memory bloat.
func (s *Store) Import(ctx context.Context, rowChan <-chan ExportRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO cache (hash, namespace, method, path, status, headers, body, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for r := range rowChan {
		createdAt := r.CreatedAt.UTC().Format(time.RFC3339Nano)
		if _, err := stmt.ExecContext(ctx, r.Hash, r.Namespace, r.Method, r.Path, r.Status, r.Headers, r.Body, createdAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
