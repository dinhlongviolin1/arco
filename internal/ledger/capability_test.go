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

func TestGrant_IdempotentNoDuplicateOrDoubleBump(t *testing.T) {
	s := newTestStore(t)
	sess := newWork(t, s)

	var r1, r2 int64
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		r1, e = tx.Grant(sess, "git.pr.merge", "cli", core.Event{Kind: "grant", SessionID: sess})
		return e
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		r2, e = tx.Grant(sess, "git.pr.merge", "cli", core.Event{Kind: "grant", SessionID: sess})
		return e
	}))
	require.Equal(t, r1, r2, "re-granting an active capability must not bump perm_rev")

	var rows int
	require.NoError(t, s.DB().QueryRow(
		`SELECT COUNT(1) FROM session_grants WHERE session_id=? AND capability=? AND status='active'`,
		sess, "git.pr.merge").Scan(&rows))
	require.Equal(t, 1, rows, "no duplicate active grant row")
}

func TestRevoke_NoOpDoesNotBumpPermRevOrEmitEvent(t *testing.T) {
	s := newTestStore(t)
	sess := newWork(t, s)
	before, _ := s.Reader().GetSession(sess)

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, e := tx.Revoke(sess, "git.pr.merge", core.Event{Kind: "revoke", SessionID: sess})
		return e
	}))
	after, _ := s.Reader().GetSession(sess)
	require.Equal(t, before.PermRev, after.PermRev, "revoking a never-granted capability must not churn perm_rev")

	var n int
	require.NoError(t, s.DB().QueryRow(`SELECT COUNT(1) FROM events WHERE kind='revoke'`).Scan(&n))
	require.Equal(t, 0, n, "no revoke event for a no-op revoke")
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

// GrantedCapabilities returns the effective set permcompile needs: default-allowed
// caps always present; an explicit grant adds a default-off cap; revoke removes
// it — mirroring Allowed() as a set.
func TestGrantedCapabilities_EffectiveSet(t *testing.T) {
	s := newTestStore(t)
	sess := newWork(t, s)

	g0, err := s.Reader().GrantedCapabilities(sess)
	require.NoError(t, err)
	require.True(t, g0["git.commit"], "default-allowed cap is granted")
	require.False(t, g0["net.fetch"], "default-off cap not granted until an explicit grant")

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, e := tx.Grant(sess, "net.fetch", "cli", core.Event{Kind: "grant", SessionID: sess})
		return e
	}))
	g1, err := s.Reader().GrantedCapabilities(sess)
	require.NoError(t, err)
	require.True(t, g1["net.fetch"], "explicit grant appears in the set")
	require.True(t, g1["git.commit"], "default caps still present")

	// consistency with Allowed(): every capability in the CURRENT set is Allowed().
	for cap := range g1 {
		ok, _ := s.Reader().Allowed(sess, cap)
		require.True(t, ok, "%s in granted set must be Allowed()", cap)
	}

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, e := tx.Revoke(sess, "net.fetch", core.Event{Kind: "revoke", SessionID: sess})
		return e
	}))
	g2, err := s.Reader().GrantedCapabilities(sess)
	require.NoError(t, err)
	require.False(t, g2["net.fetch"], "revoked grant removed from the set")
}
