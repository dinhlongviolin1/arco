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

// Regression (opus F1): even a NON-high-blast capability (net.fetch) attempted
// via the deny-listed path cannot be promoted to a standing grant — the
// escalation's recorded danger class blocks scope=session, not just the
// catalog's high_blast flag.
func TestAudit_NonHighBlastCapStillBlockedFromGrant(t *testing.T) {
	e, s, _ := newEngine(t)
	wid := dispatchRunning(t, e)
	require.NoError(t, e.AuditDeniedAttempt(context.Background(), wid, "net.fetch", "x", "evt-nf"))
	escs, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: wid})
	require.Len(t, escs, 1)
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.DecideConfirm(escs[0].ID, true, core.ScopeSession, core.Event{Kind: "confirm_dec", WorkerID: wid, SessionID: mustWorker(t, s, wid).OwnerSession, Payload: "{}"})
	})
	require.ErrorIs(t, err, core.ErrHighBlastScope, "a danger-class escalation blocks scope=session even for a non-high-blast cap")
}

// Regression (opus F2): a deny-listed attempt must SURFACE as the pending
// escalation even when a benign question was already pending (which would
// otherwise shadow it and resume the worker on being answered).
func TestAudit_DangerSurfacesPastPendingQuestion(t *testing.T) {
	e, s, _ := newEngine(t)
	wid := dispatchRunning(t, e)
	// a pre-existing benign question
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, err := tx.OpenEscalation(core.Escalation{WorkerID: wid, SessionID: mustWorker(t, s, wid).OwnerSession, Kind: "question", QuestionClass: "clarify", ActionClass: core.ClassAmbiguous, Tier: core.TierMedium, Action: "which framework?"})
		return err
	}))
	require.NoError(t, e.AuditDeniedAttempt(context.Background(), wid, "git.push.main", "x", "evt-q"))

	escs, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: wid})
	require.Len(t, escs, 1, "exactly one pending escalation")
	require.Equal(t, "confirm", escs[0].Kind, "the danger confirm surfaces, not the shadowing question")
	require.Equal(t, core.ClassDanger, escs[0].ActionClass)
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

// F5: a REJECTED danger-confirm on an audit-paused worker now transitions it to
// blocked (was silently stuck paused because paused→blocked was illegal).
func TestAudit_RejectFromPausedGoesToBlocked(t *testing.T) {
	e, s, _ := newEngine(t)
	wid := dispatchRunning(t, e)
	require.NoError(t, e.AuditDeniedAttempt(context.Background(), wid, "git.push.main", "x", "evt-r"))
	require.Equal(t, core.WorkerPaused, mustWorker(t, s, wid).State)
	escs, _ := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: wid})
	require.Len(t, escs, 1)

	// reject the danger confirm → worker should end blocked (parked), not paused
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.DecideConfirm(escs[0].ID, false, core.ScopeOnce, core.Event{Kind: "confirm_dec", WorkerID: wid, SessionID: mustWorker(t, s, wid).OwnerSession, Payload: "{}"})
	}))
	require.Equal(t, core.WorkerBlocked, mustWorker(t, s, wid).State, "rejected danger action → blocked, not stuck paused")
}
