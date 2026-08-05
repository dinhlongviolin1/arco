package ledger

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// newWork creates an open work session and returns its id.
func newWork(t *testing.T, s *Store) string {
	t.Helper()
	id := ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: id, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))
	return id
}

// newChildWork creates a child work session under parent.
func newChildWork(t *testing.T, s *Store, parent string) string {
	t.Helper()
	id := ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: id, Status: core.SessionOpen, Kind: core.SessionKindWork, ParentSession: parent})
	}))
	return id
}

// newWorker creates a starting worker owned by session and returns its id.
func newWorker(t *testing.T, s *Store, session string) string {
	t.Helper()
	id := ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateWorker(core.Worker{ID: id, OwnerSession: session, State: core.WorkerStarting, Workspace: "arco_" + id})
	}))
	return id
}
