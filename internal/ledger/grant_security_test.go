// grant_security_test.go closes the build-guide-rev6 §E "Security" debt for
// the grant path: "high-blast `Grant` rejected off the local-CLI caller class"
// and "worker cannot exercise a revoked cap after recompile (M5)".
package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/permcompile"
)

// §E: "high-blast Grant rejected off the local-CLI caller class".
//
// FINDING: caller provenance is NOT representable in the ledger Grant seam.
// Tx.Grant's signature is Grant(sessionID, capability, grantedBy string, e
// Event) — `grantedBy` is free-text attribution, not an enforced caller class,
// so the ledger layer accepts a high-blast grant from ANY caller (asserted
// below to pin the gap). The P4 "high-blast Grant = local-CLI only" precondition
// therefore rests entirely on exposure: today Grant's ONLY production caller is
// the escalation decide() path (ledger/escalations.go) — there is no grant API
// endpoint or `arco grant` CLI command yet — and that one reachable path DOES
// reject high-blast (core.ErrHighBlastScope), which this test pins as the
// current gate. When a grant endpoint/CLI lands, it must carry an enforced
// caller class and this test must be extended to reject non-CLI callers.
func TestGrantSecurity_HighBlastRejectedOffLocalCLICallerClass(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sess := newWork(t, s)
	wid := newWorker(t, s, sess)

	permRevOf := func() int64 {
		se, err := s.Reader().GetSession(sess)
		require.NoError(t, err)
		return se.PermRev
	}

	// A pending danger confirm carrying a HIGH-BLAST capability — the shape a
	// worker would use to launder a deny-listed action into a standing grant.
	var escID string
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		var err error
		escID, err = tx.OpenEscalation(core.Escalation{
			WorkerID: wid, SessionID: sess, Kind: "confirm",
			ActionClass: core.ClassDanger, Tier: core.TierHighBlast,
			Capability: "git.push.main", Action: "push to main",
		})
		return err
	}))

	// The non-CLI caller class (escalation decision) may NOT promote a standing
	// high-blast grant: scope=session is rejected, nothing is granted, no
	// perm_rev churn (the failing tx rolls back whole).
	err := s.WithTx(ctx, func(tx core.Tx) error {
		return tx.DecideConfirm(escID, true, core.ScopeSession, core.Event{Kind: "escalation_decided", SessionID: sess})
	})
	require.ErrorIs(t, err, core.ErrHighBlastScope)
	granted, gerr := s.Reader().GrantedCapabilities(sess)
	require.NoError(t, gerr)
	require.False(t, granted["git.push.main"], "the rejected decision must not leave a grant behind")
	require.Zero(t, permRevOf())

	// The same gate holds on the question path (AnswerQuestion shares decide()).
	wid2 := newWorker(t, s, sess)
	var escID2 string
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		var err error
		escID2, err = tx.OpenEscalation(core.Escalation{
			WorkerID: wid2, SessionID: sess, Kind: "question",
			ActionClass: core.ClassAmbiguous, Tier: core.TierMedium, // recorded row says medium…
			Capability: "external.deploy", Action: "may I deploy?", // …but the catalog says high-blast: the catalog wins
		})
		return err
	}))
	err = s.WithTx(ctx, func(tx core.Tx) error {
		return tx.AnswerQuestion(escID2, "go ahead", core.ScopeSession, core.Event{Kind: "escalation_answered", SessionID: sess})
	})
	require.ErrorIs(t, err, core.ErrHighBlastScope)

	// The gate must NOT block a REJECTION (a "no" never grants): the pending
	// confirm resolves rejected, still with no grant.
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.DecideConfirm(escID, false, core.ScopeSession, core.Event{Kind: "escalation_decided", SessionID: sess})
	}))
	esc, err := s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.Equal(t, "rejected", esc.Status)
	granted, err = s.Reader().GrantedCapabilities(sess)
	require.NoError(t, err)
	require.False(t, granted["git.push.main"])

	// FINDING (documents the gap, do not "fix" in a test): the ledger seam
	// itself has no caller-class check — a direct Grant of a high-blast
	// capability succeeds no matter what `grantedBy` claims. The local-CLI-only
	// property currently holds ONLY because no other caller is wired to Grant.
	var rev int64
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		var err error
		rev, err = tx.Grant(sess, "git.push.main", "tg:not-the-local-cli", core.Event{Kind: "grant", SessionID: sess})
		return err
	}))
	require.Equal(t, int64(1), rev)
	granted, err = s.Reader().GrantedCapabilities(sess)
	require.NoError(t, err)
	require.True(t, granted["git.push.main"],
		"FINDING: ledger Grant accepts a high-blast grant from any caller — provenance is unenforced at this seam")
}

// §E: "worker cannot exercise a revoked cap after recompile (M5)": grant a
// capability, compile worker settings, REVOKE it, recompile — the revoked
// capability must be absent from the newly compiled settings, and the session
// perm_rev (how worker-config staleness is detected: workers carry
// session_perm_rev and are recompiled when it lags) must bump on both the
// grant and the revoke.
func TestGrantSecurity_RevokedCapabilityAbsentAfterRecompile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sess := newWork(t, s)
	worktree := t.TempDir()

	compile := func(dir string) map[string][]string {
		t.Helper()
		granted, err := s.Reader().GrantedCapabilities(sess)
		require.NoError(t, err)
		cat, err := s.Reader().Catalog()
		require.NoError(t, err)
		require.NoError(t, permcompile.Compile(dir, worktree, granted, cat))
		return readCompiledPermissions(t, dir)
	}

	// Grant net.fetch (medium, default-off) → perm_rev 1; it compiles to `ask`
	// ("WebFetch"/"WebSearch" are its tool patterns).
	var rev int64
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		var err error
		rev, err = tx.Grant(sess, "net.fetch", "cli", core.Event{Kind: "grant", SessionID: sess})
		return err
	}))
	require.Equal(t, int64(1), rev, "grant bumps perm_rev — the staleness signal for worker recompiles")

	before := compile(t.TempDir())
	require.Contains(t, before["ask"], "WebFetch", "granted medium capability compiles to ask")
	require.NotContains(t, before["allow"], "WebFetch")

	// Revoke → perm_rev 2 → recompile: the capability is gone from the worker's
	// effective settings (neither ask nor allow — Claude default-denies unlisted).
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		var err error
		rev, err = tx.Revoke(sess, "net.fetch", core.Event{Kind: "revoke", SessionID: sess})
		return err
	}))
	require.Equal(t, int64(2), rev, "revoke bumps perm_rev again, invalidating compiled configs")
	se, err := s.Reader().GetSession(sess)
	require.NoError(t, err)
	require.Equal(t, int64(2), se.PermRev)

	after := compile(t.TempDir())
	require.NotContains(t, after["ask"], "WebFetch", "revoked capability must vanish from ask on recompile")
	require.NotContains(t, after["ask"], "WebSearch")
	require.NotContains(t, after["allow"], "WebFetch", "…and must not resurface in allow")
	require.NotContains(t, after["allow"], "WebSearch")

	// The authoritative arco-side gate agrees with the compiled artifact.
	allowed, err := s.Reader().Allowed(sess, "net.fetch")
	require.NoError(t, err)
	require.False(t, allowed, "Allowed() (the authoritative layer) also denies the revoked capability")
}
