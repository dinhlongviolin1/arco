package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestAudit_DeniedAttemptPausesAndEscalates(t *testing.T) {
	e, s, _ := newEngine(t)
	wid := dispatchRunning(t, e)

	require.NoError(t, e.AuditDeniedAttempt(context.Background(), wid, "git.push.main", "tried to push main", "evt-1"))

	w, _ := s.Reader().GetWorker(wid)
	require.Equal(t, core.WorkerPaused, w.State, "worker auto-paused on a deny-listed attempt")

	escs, err := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: wid})
	require.NoError(t, err)
	require.Len(t, escs, 1)
	require.Equal(t, "confirm", escs[0].Kind)
	require.Equal(t, core.ClassDanger, escs[0].ActionClass)
	require.Equal(t, core.TierHighBlast, escs[0].Tier)
	require.Equal(t, "git.push.main", escs[0].Capability)

	// audit_denied event recorded
	evs, _ := s.Reader().EventsSince(0, 100000)
	found := false
	for _, ev := range evs {
		if ev.Kind == "audit_denied" && ev.WorkerID == wid {
			found = true
			require.Contains(t, ev.Payload, "git.push.main")
		}
	}
	require.True(t, found, "audit_denied event recorded")
}

// Redelivery of the same attempt (same source_event_id) must not double-pause or
// open a second escalation.
func TestAudit_IdempotentOnRedelivery(t *testing.T) {
	e, s, _ := newEngine(t)
	wid := dispatchRunning(t, e)
	require.NoError(t, e.AuditDeniedAttempt(context.Background(), wid, "external.deploy", "d", "evt-9"))
	revAfterFirst := mustWorker(t, s, wid).Rev

	// same delivery id again
	require.NoError(t, e.AuditDeniedAttempt(context.Background(), wid, "external.deploy", "d", "evt-9"))
	require.Equal(t, revAfterFirst, mustWorker(t, s, wid).Rev, "no re-pause / rev churn on redelivery")

	escs, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: wid})
	require.Len(t, escs, 1, "still exactly one pending escalation")
}

// The danger escalation is high_blast, so approving it can NOT promote a standing
// session grant (a probed deny-listed capability never laundered into a grant).
func TestAudit_ConfirmCannotPromoteStandingGrant(t *testing.T) {
	e, s, _ := newEngine(t)
	wid := dispatchRunning(t, e)
	require.NoError(t, e.AuditDeniedAttempt(context.Background(), wid, "git.push.main", "x", "evt-2"))
	escs, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: wid})
	require.Len(t, escs, 1)

	// approving with scope=session on a high-blast capability is rejected
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.DecideConfirm(escs[0].ID, true, core.ScopeSession, core.Event{Kind: "confirm_dec", WorkerID: wid, SessionID: mustWorker(t, s, wid).OwnerSession, Payload: "{}"})
	})
	require.ErrorIs(t, err, core.ErrHighBlastScope)

	// once-scope approval works and resumes the (paused) worker
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.DecideConfirm(escs[0].ID, true, core.ScopeOnce, core.Event{Kind: "confirm_dec", WorkerID: wid, SessionID: mustWorker(t, s, wid).OwnerSession, Payload: "{}"})
	}))
	require.Equal(t, core.WorkerRunning, mustWorker(t, s, wid).State, "paused worker resumes on approve")
	ok, _ := s.Reader().Allowed(mustWorker(t, s, wid).OwnerSession, "git.push.main")
	require.False(t, ok, "no standing grant promoted")
}
