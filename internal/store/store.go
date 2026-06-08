// Package store is the SQLite persistence layer. It uses the pure-Go
// modernc.org/sqlite driver (no cgo) so the final binary links statically and
// runs on a scratch image. It persists poll snapshots (for the dashboard and
// history sparklines) and OAuth tokens / pending PKCE flows.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/algonode/aimon/internal/model"
	_ "modernc.org/sqlite"
)

// Store wraps a *sql.DB with aimon's queries.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path, ensures the
// parent directory exists, applies pragmas tuned for a single-writer poller,
// and runs migrations.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}

	// _pragma busy_timeout keeps concurrent reads (HTTP) from erroring while the
	// poller writes; WAL gives readers a consistent view without blocking.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single connection avoids "database is locked" churn for our tiny workload.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS snapshots (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    provider  TEXT    NOT NULL,
    label     TEXT    NOT NULL,
    ts        INTEGER NOT NULL,
    ok        INTEGER NOT NULL,
    kind      TEXT,
    unit      TEXT,
    balance   REAL,
    total     REAL,
    used      REAL,
    plan      TEXT,
    error     TEXT,
    payload   TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_id_ts ON snapshots(provider, label, ts);

CREATE TABLE IF NOT EXISTS oauth_tokens (
    provider      TEXT NOT NULL,
    label         TEXT NOT NULL,
    access_token  TEXT NOT NULL,
    refresh_token TEXT,
    expires_at    INTEGER,
    scope         TEXT,
    raw           TEXT,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (provider, label)
);

CREATE TABLE IF NOT EXISTS oauth_pending (
    state         TEXT PRIMARY KEY,
    provider      TEXT NOT NULL,
    label         TEXT NOT NULL,
    code_verifier TEXT NOT NULL,
    redirect_uri  TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// WriteSnapshot appends one poll result.
func (s *Store) WriteSnapshot(ctx context.Context, st model.Status) error {
	payload, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO snapshots (provider, label, ts, ok, kind, unit, balance, total, used, plan, error, payload)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		st.Provider, st.Label, st.UpdatedAt, b2i(st.OK), st.Kind, st.Unit,
		nf(st.Balance), nf(st.Total), nf(st.Used), st.Plan, st.Error, string(payload),
	)
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return nil
}

// LatestStatuses returns the most recent snapshot for every (provider, label).
func (s *Store) LatestStatuses(ctx context.Context) ([]model.Status, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT payload FROM snapshots
WHERE id IN (SELECT MAX(id) FROM snapshots GROUP BY provider, label)
ORDER BY provider, label`)
	if err != nil {
		return nil, fmt.Errorf("query latest: %w", err)
	}
	defer rows.Close()

	var out []model.Status
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var st model.Status
		if err := json.Unmarshal([]byte(payload), &st); err != nil {
			continue // skip a corrupt row rather than failing the whole dashboard
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Point is a single time-series sample for sparklines.
type Point struct {
	T int64   `json:"t"` // unix seconds
	V float64 `json:"v"` // value (balance for credit, used for cost/subscription)
}

// History returns up to limit samples for a monitor since the given unix time,
// oldest first. The value chosen reflects the monitor kind.
func (s *Store) History(ctx context.Context, provider, label string, since int64, limit int) ([]Point, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ts, kind, balance, used, total FROM snapshots
WHERE provider = ? AND label = ? AND ts >= ? AND ok = 1
ORDER BY ts ASC
LIMIT ?`, provider, label, since, limit)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	var out []Point
	for rows.Next() {
		var ts int64
		var kind sql.NullString
		var balance, used, total sql.NullFloat64
		if err := rows.Scan(&ts, &kind, &balance, &used, &total); err != nil {
			return nil, err
		}
		v, ok := pickValue(kind.String, balance, used, total)
		if !ok {
			continue
		}
		out = append(out, Point{T: ts, V: v})
	}
	return out, rows.Err()
}

// pickValue chooses the sparkline value for a row based on monitor kind.
func pickValue(kind string, balance, used, total sql.NullFloat64) (float64, bool) {
	switch kind {
	case model.KindCredit:
		if balance.Valid {
			return balance.Float64, true
		}
	case model.KindCost, model.KindSubscription:
		if used.Valid {
			return used.Float64, true
		}
	}
	switch {
	case balance.Valid:
		return balance.Float64, true
	case used.Valid:
		return used.Float64, true
	case total.Valid:
		return total.Float64, true
	}
	return 0, false
}

// Prune deletes snapshots older than the cutoff to keep the DB bounded.
func (s *Store) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE ts < ?`, olderThan.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nf(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
