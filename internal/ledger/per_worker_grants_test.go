package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// Issue-model isolation: a per-worker grant applies ONLY to that worker; a
// session-wide grant (worker_id NULL) applies to all; both can coexist for the
// same capability (the migration's COALESCE unique index).
func TestGrantWorker_IsolatedBetweenWorkersButSessionWideShared(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sess := newWork(t, s)
	wa := newWorker(t, s, sess)
	wb := newWorker(t, s, sess)

	// per-worker grant to A only (net.fetch is non-default, non-high-blast).
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		_, err := tx.GrantWorker(sess, wa, "net.fetch", "human", core.Event{Kind: "grant", SessionID: sess, WorkerID: wa})
		return err
	}))
	ga, err := s.Reader().GrantedCapabilitiesForWorker(sess, wa)
	require.NoError(t, err)
	gb, err := s.Reader().GrantedCapabilitiesForWorker(sess, wb)
	require.NoError(t, err)
	require.True(t, ga["net.fetch"], "worker A sees its own per-worker grant")
	require.False(t, gb["net.fetch"], "worker B does NOT inherit A's per-worker grant (isolation)")

	// a session-wide grant reaches BOTH workers.
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		_, err := tx.Grant(sess, "git.push.shared", "human", core.Event{Kind: "grant", SessionID: sess})
		return err
	}))
	ga2, _ := s.Reader().GrantedCapabilitiesForWorker(sess, wa)
	gb2, _ := s.Reader().GrantedCapabilitiesForWorker(sess, wb)
	require.True(t, ga2["git.push.shared"], "session-wide baseline reaches A")
	require.True(t, gb2["git.push.shared"], "session-wide baseline reaches B")

	// a session-wide grant for the SAME cap coexists with A's per-worker one
	// (COALESCE unique index) and now reaches B too.
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		_, err := tx.Grant(sess, "net.fetch", "human", core.Event{Kind: "grant", SessionID: sess})
		return err
	}))
	gb3, _ := s.Reader().GrantedCapabilitiesForWorker(sess, wb)
	require.True(t, gb3["net.fetch"], "a session-wide net.fetch reaches B, coexisting with A's per-worker grant")

	// the plain session-wide GrantedCapabilities is unchanged (backward compat).
	sw, err := s.Reader().GrantedCapabilities(sess)
	require.NoError(t, err)
	require.True(t, sw["git.push.shared"], "session-wide view still works")
}
