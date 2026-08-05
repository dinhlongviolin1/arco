// Package ledger is the SQLite adapter implementing core.Store. It is the only
// package that imports a storage engine; everything else speaks core's ports.
//
// Concurrency (build-guide Global Constraints): WAL allows concurrent reads;
// all writes serialize through a single-writer mutex, so WithTx callbacks never
// interleave. Post-commit work (reconcile-enqueue, broadcast) is the caller's
// responsibility AFTER WithTx returns — never reentrant from inside fn.
package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dinhlongviolin1/arco/internal/core"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is the SQLite-backed core.Store.
type Store struct {
	db    *sql.DB
	wmu   sync.Mutex // single-writer serialization
	clock core.Clock
	scrub core.Scrubber // write-time secret redaction (nil = none)
}

// SetScrubber installs a write-time redactor applied to every event payload at
// the single insert chokepoint (build-guide B4). Set once at startup.
func (s *Store) SetScrubber(sc core.Scrubber) { s.scrub = sc }

// SetClock overrides the time source (default time.Now). Injectable so tests can
// drive TTL / cooldown / start-rate windows deterministically. Set once, before
// concurrent use — the field is read without a lock.
func (s *Store) SetClock(c core.Clock) { s.clock = c }

var _ core.Store = (*Store)(nil)

// Open opens (creating if needed) the SQLite database at path with WAL and
// foreign keys on. Use ":memory:" style paths only via OpenDSN for tests.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=journal_mode(WAL)"
	return openDSN(dsn)
}

func openDSN(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, clock: time.Now}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for tests/tools in the same module. Not part of the
// core.Store port (the port never leaks *sql.DB).
func (s *Store) DB() *sql.DB { return s.db }

type migration struct {
	version int
	name    string
	sqlText string
	sum     string
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationsFS.ReadFile(filepath.ToSlash(filepath.Join("migrations", e.Name())))
		if err != nil {
			return nil, err
		}
		// filename: NNNN_name.sql
		base := strings.TrimSuffix(e.Name(), ".sql")
		numPart, namePart, _ := strings.Cut(base, "_")
		v, err := strconv.Atoi(numPart)
		if err != nil {
			return nil, fmt.Errorf("ledger: bad migration filename %q: %w", e.Name(), err)
		}
		sum := sha256.Sum256(b)
		ms = append(ms, migration{version: v, name: namePart, sqlText: string(b), sum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// Migrate applies unapplied migrations in version order and records each in
// schema_migrations (checksum = sha256 of the migration file bytes). It is the
// SOLE writer of the bookkeeping row; migration files never self-INSERT.
func (s *Store) Migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		return fmt.Errorf("ledger: no embedded migrations found")
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if got, ok := applied[m.version]; ok {
			if got != m.sum {
				return fmt.Errorf("ledger: migration %d checksum drift: recorded=%s file=%s", m.version, got, m.sum)
			}
			continue
		}
		// Apply the DDL AND the bookkeeping row in ONE transaction, so a crash
		// mid-migration never leaves tables without a schema_migrations row (which
		// would re-run the file on next boot and collide). PRAGMAs live in the DSN,
		// not the file, so DDL is transaction-safe.
		if err := func() error {
			mtx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			if _, err := mtx.ExecContext(ctx, m.sqlText); err != nil {
				_ = mtx.Rollback()
				return err
			}
			if _, err := mtx.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?,?,?,?)`,
				m.version, m.name, m.sum, s.now()); err != nil {
				_ = mtx.Rollback()
				return err
			}
			return mtx.Commit()
		}(); err != nil {
			return fmt.Errorf("ledger: migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

func (s *Store) appliedVersions(ctx context.Context) (map[int]string, error) {
	// On a fresh DB the table doesn't exist yet (migration 0001 creates it).
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return map[int]string{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, err
		}
		out[v] = sum
	}
	return out, rows.Err()
}

// WithTx runs fn inside a serialized single-writer transaction. It is
// synchronous; it commits on nil error, rolls back otherwise.
func (s *Store) WithTx(ctx context.Context, fn func(core.Tx) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	tx := newTxn(sqlTx, s.now, s.scrub)
	if err := fn(tx); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	return sqlTx.Commit()
}

// Reader returns a read-only view over the database (WAL concurrent reads).
func (s *Store) Reader() core.Reader { return &reader{q: s.db} }

func (s *Store) now() string { return s.clock().UTC().Format(time.RFC3339Nano) }

// querier is satisfied by *sql.DB (reads) and *sql.Tx (reads within a write tx).
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
