package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestAllowed_DefaultTreeAndUnknownFailClosed(t *testing.T) {
	s := newTestStore(t)
	sess := newWork(t, s)

	ok, err := s.Reader().Allowed(sess, "git.commit") // default-allowed
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = s.Reader().Allowed(sess, "git.push.main") // high-blast, no grant
	require.NoError(t, err)
	require.False(t, ok)

	ok, err = s.Reader().Allowed(sess, "totally.unknown") // unclassifiable → fail closed
	require.NoError(t, err)
	require.False(t, ok)
}

func TestGrantThenRevoke_FlipsAllowedAndBumpsPermRev(t *testing.T) {
	s := newTestStore(t)
	sess := newWork(t, s)

	var permRev int64
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		r, e := tx.Grant(sess, "git.pr.merge", "cli", core.Event{Kind: "grant", SessionID: sess})
		permRev = r
		return e
	}))
	require.Equal(t, int64(1), permRev)

	ok, err := s.Reader().Allowed(sess, "git.pr.merge")
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, e := tx.Revoke(sess, "git.pr.merge", core.Event{Kind: "revoke", SessionID: sess})
		return e
	}))
	ok, err = s.Reader().Allowed(sess, "git.pr.merge")
	require.NoError(t, err)
	require.False(t, ok)

	got, _ := s.Reader().GetSession(sess)
	require.GreaterOrEqual(t, got.PermRev, int64(2)) // bumped by grant and revoke
}

// Revoke on a parent cascades to descendant sessions (child ⊆ parent).
func TestRevoke_CascadesToSubtree(t *testing.T) {
	s := newTestStore(t)
	parent := newWork(t, s)
	child := newChildWork(t, s, parent)

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if _, e := tx.Grant(parent, "git.pr.merge", "cli", core.Event{Kind: "grant", SessionID: parent}); e != nil {
			return e
		}
		_, e := tx.Grant(child, "git.pr.merge", "cli", core.Event{Kind: "grant", SessionID: child})
		return e
	}))
	okC, _ := s.Reader().Allowed(child, "git.pr.merge")
	require.True(t, okC)

	// revoke on the PARENT must cascade to the child
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, e := tx.Revoke(parent, "git.pr.merge", core.Event{Kind: "revoke", SessionID: parent})
		return e
	}))
	okC, _ = s.Reader().Allowed(child, "git.pr.merge")
	require.False(t, okC, "revoke on parent must cascade to descendant grants")
}
