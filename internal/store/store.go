// Package store is the SQLite persistence layer.
//
// It uses the pure-Go modernc.org/sqlite driver so the binary builds with
// CGO_ENABLED=0 and depends on no system libsqlite3.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the database handle.
type Store struct {
	db       *sql.DB
	readOnly bool
}

const schema = `
CREATE TABLE IF NOT EXISTS targets (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  addr          TEXT NOT NULL,
  tier          TEXT NOT NULL,
  baseline_loss REAL NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS samples (
  ts        INTEGER NOT NULL,
  target_id INTEGER NOT NULL REFERENCES targets(id),
  ok        INTEGER NOT NULL,
  rtt_us    INTEGER,
  loss_pct  REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_ts ON samples(ts);
-- Raw samples are kept indefinitely, so the per-target time-series queries that
-- back the latency chart need their own index; without it they degrade into a
-- full scan that gets slower every day the tool runs.
CREATE INDEX IF NOT EXISTS idx_samples_target_ts ON samples(target_id, ts);

-- One row per probe cycle. Doubles as the heartbeat: a gap in this table means
-- the collector was not running, which must never be counted as an outage.
CREATE TABLE IF NOT EXISTS cycles (
  ts      INTEGER PRIMARY KEY,
  verdict TEXT NOT NULL,
  iface   TEXT,
  ssid    TEXT
);

CREATE TABLE IF NOT EXISTS outages (
  id         INTEGER PRIMARY KEY,
  started    INTEGER NOT NULL,
  ended      INTEGER,
  duration_s INTEGER,
  class      TEXT NOT NULL,
  detail     TEXT
);
CREATE INDEX IF NOT EXISTS idx_outages_started ON outages(started);

CREATE TABLE IF NOT EXISTS daily_rollup (
  day     TEXT NOT NULL,
  verdict TEXT NOT NULL,
  seconds INTEGER NOT NULL,
  PRIMARY KEY (day, verdict)
);

CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
`

// Open opens the database read-write and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// One writer only; more connections just create lock contention.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// OpenReadOnly opens the database for reading only. The dashboard uses this so
// it is structurally incapable of locking or corrupting the collector's data.
func OpenReadOnly(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	return &Store{db: db, readOnly: true}, nil
}

// Close closes the underlying handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the report package's queries.
func (s *Store) DB() *sql.DB { return s.db }

// UpsertTarget inserts or updates a target and returns its id.
func (s *Store) UpsertTarget(name, addr, tier string) (int64, error) {
	_, err := s.db.Exec(
		`INSERT INTO targets (name, addr, tier) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET addr=excluded.addr, tier=excluded.tier`,
		name, addr, tier)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRow(`SELECT id FROM targets WHERE name = ?`, name).Scan(&id)
	return id, err
}

// SetBaselineLoss records a target's measured idle loss rate, used to spot
// routers that de-prioritise ICMP.
func (s *Store) SetBaselineLoss(name string, pct float64) error {
	_, err := s.db.Exec(`UPDATE targets SET baseline_loss = ? WHERE name = ?`, pct, name)
	return err
}

// Sample is one target's result within a cycle.
type Sample struct {
	TargetID int64
	OK       bool
	RTT      time.Duration
	LossPct  float64
}

// WriteCycle records a completed probe cycle and its samples in one
// transaction, so a crash can never leave samples without their heartbeat.
func (s *Store) WriteCycle(ts time.Time, verdict, iface, ssid string, samples []Sample) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	unix := ts.Unix()
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO cycles (ts, verdict, iface, ssid) VALUES (?, ?, ?, ?)`,
		unix, verdict, iface, ssid); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT INTO samples (ts, target_id, ok, rtt_us, loss_pct) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, sm := range samples {
		var rtt interface{}
		if sm.RTT > 0 {
			rtt = sm.RTT.Microseconds()
		}
		okInt := 0
		if sm.OK {
			okInt = 1
		}
		if _, err := stmt.Exec(unix, sm.TargetID, okInt, rtt, sm.LossPct); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OpenOutage starts an outage record and returns its id.
func (s *Store) OpenOutage(started time.Time, class, detail string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO outages (started, class, detail) VALUES (?, ?, ?)`,
		started.Unix(), class, detail)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CloseOutage finalises an outage with its end time, duration and final class.
//
// The class is rewritten on close because a session can escalate: an outage
// that first looks like local trouble but later proves to be the provider must
// end up recorded against the provider.
func (s *Store) CloseOutage(id int64, ended time.Time, class string) error {
	_, err := s.db.Exec(
		`UPDATE outages SET ended = ?, duration_s = ? - started, class = ? WHERE id = ?`,
		ended.Unix(), ended.Unix(), class, id)
	return err
}

// UnfinishedOutage returns the id and start of an outage left open by a crash,
// or ok=false if there is none.
func (s *Store) UnfinishedOutage() (id int64, started time.Time, class string, ok bool) {
	var unix int64
	err := s.db.QueryRow(
		`SELECT id, started, class FROM outages WHERE ended IS NULL ORDER BY started DESC LIMIT 1`).
		Scan(&id, &unix, &class)
	if err != nil {
		return 0, time.Time{}, "", false
	}
	return id, time.Unix(unix, 0), class, true
}

// SetMeta stores a key/value pair (used for discovered topology).
func (s *Store) SetMeta(k, v string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// GetMeta reads a key, returning "" if absent.
func (s *Store) GetMeta(k string) string {
	var v string
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v); err != nil {
		return ""
	}
	return v
}

// Prune deletes raw samples older than the retention window.
//
// A retain of zero disables pruning entirely, which is the default: nothing is
// ever deleted. Cycles, outages and rollups are unconditionally kept forever.
func (s *Store) Prune(retain time.Duration) (int64, error) {
	if retain <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-retain).Unix()
	res, err := s.db.Exec(`DELETE FROM samples WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Rollup recomputes daily per-verdict totals for the last few days, so long
// report windows do not have to scan the whole cycles table.
func (s *Store) Rollup(interval time.Duration, days int) error {
	since := time.Now().AddDate(0, 0, -days).Unix()
	secs := int64(interval.Seconds())
	_, err := s.db.Exec(`
		INSERT INTO daily_rollup (day, verdict, seconds)
		SELECT date(ts, 'unixepoch', 'localtime') AS day, verdict, COUNT(*) * ?
		FROM cycles WHERE ts >= ?
		GROUP BY day, verdict
		ON CONFLICT(day, verdict) DO UPDATE SET seconds = excluded.seconds`, secs, since)
	return err
}
