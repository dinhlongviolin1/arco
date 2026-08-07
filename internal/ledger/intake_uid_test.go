package ledger

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// Guideline test (rev7/T1.6): workers.intake_uid round-trips through the
// ledger — set at spawn, NULL for legacy/cross-VM rows. Do not weaken.

func TestWorker_IntakeUIDRoundTrip(t *testing.T) {
	s := newTestStore(t)
	session := newWork(t, s)

	withUID, withoutUID := ulid.Make().String(), ulid.Make().String()
	uid := 4242
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateWorker(core.Worker{ID: withUID, OwnerSession: session, State: core.WorkerStarting,
			Workspace: "arco_" + withUID, IntakeUID: &uid})
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateWorker(core.Worker{ID: withoutUID, OwnerSession: session, State: core.WorkerStarting,
			Workspace: "arco_" + withoutUID})
	}))

	w, err := s.Reader().GetWorker(withUID)
	require.NoError(t, err)
	require.NotNil(t, w.IntakeUID)
	require.Equal(t, 4242, *w.IntakeUID)

	w2, err := s.Reader().GetWorker(withoutUID)
	require.NoError(t, err)
	require.Nil(t, w2.IntakeUID)
}
