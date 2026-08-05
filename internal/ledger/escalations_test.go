package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// waitingWorker returns a worker parked in waiting_for_user under a work session.
func waitingWorker(t *testing.T, s *Store) (worker, session string) {
	t.Helper()
	session = newWork(t, s)
	worker = newWorker(t, s, session)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.TransitionWorker(worker, core.WorkerRunning, 0, core.Event{Kind: "state_change", WorkerID: worker})
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, _ := tx.GetWorker(worker)
		return tx.TransitionWorker(worker, core.WorkerWaitingForUser, w.Rev, core.Event{Kind: "question_req", WorkerID: worker})
	}))
	return worker, session
}

func TestOpenEscalation_OnePendingPerWorker(t *testing.T) {
	s := newTestStore(t)
	worker, session := waitingWorker(t, s)
	var id1, id2 string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		id1, e = tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "question", Action: "q1"})
		return e
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		id2, e = tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "question", Action: "q2"})
		return e
	}))
	require.Equal(t, id1, id2, "a second open for the same worker must return the existing pending id")

	pending, err := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: worker})
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func TestAnswerQuestion_ResumesWorkerAndGrantsOnSessionScope(t *testing.T) {
	s := newTestStore(t)
	worker, session := waitingWorker(t, s)
	var id string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		id, e = tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "question",
			Capability: "git.pr.merge", Action: "may I merge?"})
		return e
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AnswerQuestion(id, "yes, merge it", core.ScopeSession, core.Event{Kind: "question_esc", WorkerID: worker})
	}))

	esc, _ := s.Reader().GetEscalation(id)
	require.Equal(t, "answered", esc.Status)
	require.Equal(t, "human", esc.DecidedBy)

	w, _ := s.Reader().GetWorker(worker)
	require.Equal(t, core.WorkerRunning, w.State, "answering resumes the worker")

	ok, _ := s.Reader().Allowed(session, "git.pr.merge")
	require.True(t, ok, "scope=session promotes a standing grant for a non-high-blast cap")

	require.Equal(t, "always", esc.OnceOrAlways, "a real grant records always")
	require.NotEmpty(t, esc.ResumedAt, "resumed_at is stamped on resume")
}

// A rejection of a high-blast confirm with scope=session must NOT be blocked by
// the high-blast gate (a reject never grants) — the rejection must land.
func TestDecideConfirm_RejectHighBlastSessionScopeStillLands(t *testing.T) {
	s := newTestStore(t)
	worker, session := waitingWorker(t, s)
	var id string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		id, e = tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "confirm",
			Capability: "git.push.main", ActionClass: core.ClassDanger, Tier: core.TierHighBlast, Action: "push main?"})
		return e
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.DecideConfirm(id, false, core.ScopeSession, core.Event{Kind: "confirm_dec", WorkerID: worker})
	}))
	esc, _ := s.Reader().GetEscalation(id)
	require.Equal(t, "rejected", esc.Status)
	require.Equal(t, "once", esc.OnceOrAlways, "a rejection grants nothing")
	w, _ := s.Reader().GetWorker(worker)
	require.Equal(t, core.WorkerBlocked, w.State)
}

func TestAnswerQuestion_UnknownCapSessionScopeFailsClosed(t *testing.T) {
	s := newTestStore(t)
	worker, session := waitingWorker(t, s)
	var id string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		id, e = tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "question",
			Capability: "made.up.capability", Action: "?"})
		return e
	}))
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AnswerQuestion(id, "ok", core.ScopeSession, core.Event{Kind: "question_esc", WorkerID: worker})
	})
	require.Error(t, err, "granting an unknown capability via an escalation must fail closed")
	esc, _ := s.Reader().GetEscalation(id)
	require.Equal(t, "pending", esc.Status)
}

func TestOpenEscalation_OnePendingAcrossKinds(t *testing.T) {
	s := newTestStore(t)
	worker, session := waitingWorker(t, s)
	var idQ, idC string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		idQ, e = tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "question", Action: "q"})
		return e
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		idC, e = tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "confirm", Capability: "git.pr.merge", Action: "c"})
		return e
	}))
	require.Equal(t, idQ, idC, "a confirm must not open while a question is pending for the same worker")
}

func TestAnswerQuestion_HighBlastScopeRejected(t *testing.T) {
	s := newTestStore(t)
	worker, session := waitingWorker(t, s)
	var id string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		id, e = tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "question",
			Capability: "git.push.main", Action: "push to main?"})
		return e
	}))
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AnswerQuestion(id, "sure", core.ScopeSession, core.Event{Kind: "question_esc", WorkerID: worker})
	})
	require.ErrorIs(t, err, core.ErrHighBlastScope)

	// nothing changed: still pending, worker still waiting, no grant
	esc, _ := s.Reader().GetEscalation(id)
	require.Equal(t, "pending", esc.Status)
	w, _ := s.Reader().GetWorker(worker)
	require.Equal(t, core.WorkerWaitingForUser, w.State)
	ok, _ := s.Reader().Allowed(session, "git.push.main")
	require.False(t, ok)
}

func TestDecideConfirm_ApproveResumes_RejectBlocks(t *testing.T) {
	s := newTestStore(t)
	// approve
	w1, sess1 := waitingWorker(t, s)
	var idA string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		idA, e = tx.OpenEscalation(core.Escalation{WorkerID: w1, SessionID: sess1, Kind: "confirm",
			ActionClass: core.ClassDanger, Tier: core.TierHighBlast, Action: "deploy?"})
		return e
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.DecideConfirm(idA, true, core.ScopeOnce, core.Event{Kind: "confirm_dec", WorkerID: w1})
	}))
	escA, _ := s.Reader().GetEscalation(idA)
	require.Equal(t, "approved", escA.Status)
	wA, _ := s.Reader().GetWorker(w1)
	require.Equal(t, core.WorkerRunning, wA.State)

	// reject → worker blocked
	w2, sess2 := waitingWorker(t, s)
	var idR string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		idR, e = tx.OpenEscalation(core.Escalation{WorkerID: w2, SessionID: sess2, Kind: "confirm", Action: "rm -rf?"})
		return e
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.DecideConfirm(idR, false, core.ScopeOnce, core.Event{Kind: "confirm_dec", WorkerID: w2})
	}))
	escR, _ := s.Reader().GetEscalation(idR)
	require.Equal(t, "rejected", escR.Status)
	wR, _ := s.Reader().GetWorker(w2)
	require.Equal(t, core.WorkerBlocked, wR.State)
}

// Disjointness: a brain DraftAnswer must never become the decision.
func TestDecide_DraftNeverBecomesDecision(t *testing.T) {
	s := newTestStore(t)
	worker, session := waitingWorker(t, s)
	var id string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		id, e = tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "question",
			DraftAnswer: "BRAIN SAYS: rebase onto main", BrainRationale: "drafted", Action: "how to proceed?"})
		return e
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AnswerQuestion(id, "HUMAN SAYS: squash instead", core.ScopeOnce, core.Event{Kind: "question_esc", WorkerID: worker})
	}))
	esc, _ := s.Reader().GetEscalation(id)
	require.Equal(t, "human", esc.DecidedBy)
	require.Equal(t, "HUMAN SAYS: squash instead", esc.AnswerText)
	require.Equal(t, "BRAIN SAYS: rebase onto main", esc.DraftAnswer, "draft is preserved, separate from the decision")
	require.NotEqual(t, esc.DraftAnswer, esc.AnswerText)
}

func TestDecide_WrongStateRejected(t *testing.T) {
	s := newTestStore(t)
	worker, session := waitingWorker(t, s)
	var id string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var e error
		id, e = tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "question", Action: "q"})
		return e
	}))
	// answering a question via the confirm path is rejected
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.DecideConfirm(id, true, core.ScopeOnce, core.Event{Kind: "confirm_dec", WorkerID: worker})
	})
	require.ErrorIs(t, err, core.ErrEscalationState)
}
