package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// newTestStore opens a fresh migrated store on a temp-file DB.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrate_FreezeInvariants(t *testing.T) {
	s := newTestStore(t)
	db := s.DB()

	// schema_migrations records version 1 with checksum == sha256(embedded file).
	b, err := migrationsFS.ReadFile("migrations/0001_init.sql")
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	var name, checksum string
	require.NoError(t, db.QueryRow(`SELECT name, checksum FROM schema_migrations WHERE version=1`).Scan(&name, &checksum))
	require.Equal(t, "init", name)
	require.Equal(t, hex.EncodeToString(sum[:]), checksum)

	// exactly one kind='pool' row, with the well-known ULID.
	var poolCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(1) FROM sessions WHERE kind='pool'`).Scan(&poolCount))
	require.Equal(t, 1, poolCount)
	var poolID string
	require.NoError(t, db.QueryRow(`SELECT id FROM sessions WHERE kind='pool'`).Scan(&poolID))
	require.Equal(t, core.PoolSessionID, poolID)
	require.Len(t, poolID, 26, "pool ULID must be 26 Crockford chars")

	// capability_catalog invariant: every high_blast row is NOT compiled onto a worker.
	var bad int
	require.NoError(t, db.QueryRow(`SELECT COUNT(1) FROM capability_catalog WHERE high_blast=1 AND compiled_worker=1`).Scan(&bad))
	require.Equal(t, 0, bad, "no high_blast capability may be compiled onto a worker")

	// DefaultTree() == the default_allowed=1 rows.
	tree, err := s.Reader().DefaultTree()
	require.NoError(t, err)
	var defCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(1) FROM capability_catalog WHERE default_allowed=1`).Scan(&defCount))
	require.Equal(t, defCount, len(tree))

	// default-off capabilities (rev-4.1 / rev-6).
	for _, cap := range []string{"net.fetch", "spawn.subworker", "memory.cross-project",
		"fleet.claim", "fleet.transfer", "fleet.move", "git.push.main", "external.deploy", "secrets.read"} {
		row, ok, err := s.Reader().Capability(cap)
		require.NoError(t, err)
		require.True(t, ok, "capability %s must be seeded", cap)
		require.False(t, row.DefaultAllowed, "capability %s must be default-off", cap)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.Migrate(context.Background()))
	require.NoError(t, s.Migrate(context.Background())) // second run is a no-op
	var n int
	require.NoError(t, s.DB().QueryRow(`SELECT COUNT(1) FROM schema_migrations`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestMigrate_ReopenExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	s1, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s1.Migrate(context.Background()))
	require.NoError(t, s1.Close())

	s2, err := Open(path)
	require.NoError(t, err)
	defer s2.Close()
	require.NoError(t, s2.Migrate(context.Background())) // checksum matches → no-op, no drift error
}

// TestWithTx_SingleWriterSerializes fires 50 concurrent write transactions; the
// single-writer mutex must serialize them so all 50 land with no lock errors.
func TestWithTx_SingleWriterSerializes(t *testing.T) {
	s := newTestStore(t)
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.WithTx(context.Background(), func(tx core.Tx) error {
				_, _, _, err := tx.AppendEvent(core.Event{Kind: "note", Payload: "{}"})
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var count int
	require.NoError(t, s.DB().QueryRow(`SELECT COUNT(1) FROM events WHERE kind='note'`).Scan(&count))
	require.Equal(t, n, count)
}
