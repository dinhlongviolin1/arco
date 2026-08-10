package reconcile

// GUIDELINE TESTS — rev7 T3.5 (autonomy earn-out: the promotion gate matrix).
//
// Pinned seam (on Engine):
//   VerificationLive    bool    — the daemon sets this true ONLY when a
//                                 verification leg is enabled (T3.1
//                                 ci_check_runs or T3.2 merge_queue).
//   EarnOutMinDecisions int     — minimum human decisions on drafts of a class.
//   EarnOutMinAgreement float64 — minimum agree/total ratio for that class.
//
// Pinned semantics:
//   - Sweep may resolve a pending drafted QUESTION with the brain's own draft,
//     recorded as answered_by "brain", ONLY when EVERY gate passes:
//     session mode auto ∧ VerificationLive ∧ non-empty draft ∧ the class's
//     human history at/above both thresholds. The resumed agent receives the
//     draft text exactly as a human answer would be delivered.
//   - CONFIRMS are NEVER auto-answered, whatever the stats — approval stays
//     human (a brain-sourced approval/grant must not exist; see ports.go's
//     grant rule).
//   - An auto-answer never creates a grant and never feeds the agreement
//     tally (it would otherwise ratify itself).

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// parkDrafted binds a pane, parks the worker in the right waiting state, and
// opens a pending escalation carrying the brain's draft.
func parkDrafted(t *testing.T, s *ledger.Store, id, ref, kind, class, draft string) string {
	t.Helper()
	waiting := core.WorkerWaitingForUser
	if kind == "confirm" {
		waiting = core.WorkerWaitingConfirmation
	}
	var escID string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.BindLaunch(id, "/wt/eo-"+ref, "base", ref, "term_"+ref); err != nil {
			return err
		}
		w, err := tx.GetWorker(id)
		if err != nil {
			return err
		}
		if err := tx.TransitionWorker(id, waiting, w.Rev, core.Event{
			Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"}); err != nil {
			return err
		}
		var e2 error
		escID, e2 = tx.OpenEscalation(core.Escalation{
			WorkerID: id, SessionID: w.OwnerSession, Kind: kind,
			QuestionClass: class, Action: "step?", DraftAnswer: draft,
		})
		return e2
	}))
	return escID
}

// seedDraftDecisions builds a class's human track record: `agree` matching and
// `disagree` differing answers to drafted questions, each on a fresh worker.
func seedDraftDecisions(t *testing.T, e *Engine, s *ledger.Store, class string, agree, disagree int) {
	t.Helper()
	for i := 0; i < agree+disagree; i++ {
		id := mkRunning(t, e, s, fmt.Sprintf("/wt/eo-seed-%s-%d", class, i), "base")
		escID := parkDrafted(t, s, id, fmt.Sprintf("eoS%d:p", i), "question", class, "go with plan A")
		ans := "go with plan A"
		if i >= agree {
			ans = "no — plan B"
		}
		require.NoError(t, e.AnswerQuestion(context.Background(), escID, ans, core.ScopeOnce))
		e.Exec.Wait()
	}
}

// newEarnedOut returns an engine where class "clarify" has fully earned out
// and every promotion gate passes except the ones a test then breaks.
func newEarnedOut(t *testing.T) (*Engine, *ledger.Store, *vm.Fake) {
	t.Helper()
	e, s, fake := newEngine(t)
	e.MissThreshold = 100 // liveness noise must not finalize parked workers here
	e.VerificationLive = true
	e.EarnOutMinDecisions = 1
	e.EarnOutMinAgreement = 1.0
	seedDraftDecisions(t, e, s, "clarify", 1, 0)
	return e, s, fake
}

func TestEarnOut_SweepAutoAnswersEarnedOutQuestion(t *testing.T) {
	e, s, fake := newEarnedOut(t)
	id := mkRunning(t, e, s, "/wt/eo-main", "base")
	setMode(t, s, id, core.ModeAuto)
	escID := parkDrafted(t, s, id, "eoQ:p1", "question", "clarify", "use the blue pipeline")

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()

	esc, err := s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.Equal(t, "answered", esc.Status, "earned-out drafted question resolved by the sweep")
	require.Equal(t, "brain", esc.AnsweredBy, "the resolution is attributed to the brain, not a human")
	require.Equal(t, core.WorkerRunning, mustWorker(t, s, id).State, "the worker resumes")
	p, ok := promptTo(fake.Prompts(), "eoQ:p1")
	require.True(t, ok, "the draft is delivered to the resumed agent's pane")
	require.Contains(t, p.Text, "use the blue pipeline")

	var grants int
	require.NoError(t, s.DB().QueryRow(`SELECT COUNT(1) FROM grants`).Scan(&grants))
	require.Zero(t, grants, "a brain auto-answer must never create a grant")

	agree, total, err := s.Reader().DraftAgreement("clarify")
	require.NoError(t, err)
	require.Equal(t, 1, agree)
	require.Equal(t, 1, total, "the auto-answer must not feed its own tally")
}

func TestEarnOut_GateMatrix_AnyFailedGateBlocksAutoAnswer(t *testing.T) {
	cases := []struct {
		name string
		prep func(t *testing.T, e *Engine, s *ledger.Store, id string)
	}{
		{"mode assist", func(t *testing.T, e *Engine, s *ledger.Store, id string) {
			setMode(t, s, id, core.ModeAssist)
		}},
		{"mode manual", func(t *testing.T, e *Engine, s *ledger.Store, id string) {
			setMode(t, s, id, core.ModeManual)
		}},
		{"verification not live", func(t *testing.T, e *Engine, s *ledger.Store, id string) {
			setMode(t, s, id, core.ModeAuto)
			e.VerificationLive = false
		}},
		{"below decision threshold", func(t *testing.T, e *Engine, s *ledger.Store, id string) {
			setMode(t, s, id, core.ModeAuto)
			e.EarnOutMinDecisions = 2 // only 1 human decision seeded
		}},
		{"below agreement threshold", func(t *testing.T, e *Engine, s *ledger.Store, id string) {
			setMode(t, s, id, core.ModeAuto)
			e.EarnOutMinAgreement = 0.9
			seedDraftDecisions(t, e, s, "clarify", 0, 1) // history now 1/2 = 0.5
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, s, fake := newEarnedOut(t)
			id := mkRunning(t, e, s, "/wt/eo-main", "base")
			tc.prep(t, e, s, id)
			escID := parkDrafted(t, s, id, "eoQ:p1", "question", "clarify", "use the blue pipeline")

			_, err := e.Sweep(context.Background())
			require.NoError(t, err)
			e.Exec.Wait()

			esc, err := s.Reader().GetEscalation(escID)
			require.NoError(t, err)
			require.Equal(t, "pending", esc.Status, "a failed gate must leave the escalation for the human")
			require.Equal(t, core.WorkerWaitingForUser, mustWorker(t, s, id).State)
			_, ok := promptTo(fake.Prompts(), "eoQ:p1")
			require.False(t, ok, "nothing is delivered to a worker the sweep did not resume")
		})
	}
}

func TestEarnOut_NoDraftNeverAutoAnswered(t *testing.T) {
	e, s, fake := newEarnedOut(t)
	id := mkRunning(t, e, s, "/wt/eo-main", "base")
	setMode(t, s, id, core.ModeAuto)
	escID := parkDrafted(t, s, id, "eoQ:p1", "question", "clarify", "")

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()

	esc, err := s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.Equal(t, "pending", esc.Status, "there is no draft to answer with")
	_, ok := promptTo(fake.Prompts(), "eoQ:p1")
	require.False(t, ok)
}

func TestEarnOut_ConfirmsNeverAutoAnswered(t *testing.T) {
	// Every gate passes and the class is fully earned out — but the escalation
	// is a CONFIRM. Approval stays human, whatever the stats.
	e, s, fake := newEarnedOut(t)
	id := mkRunning(t, e, s, "/wt/eo-main", "base")
	setMode(t, s, id, core.ModeAuto)
	escID := parkDrafted(t, s, id, "eoC:p1", "confirm", "clarify", "safe to proceed")

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()

	esc, err := s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.Equal(t, "pending", esc.Status, "a confirm is never auto-approved")
	require.Empty(t, esc.AnsweredBy)
	require.Equal(t, core.WorkerWaitingConfirmation, mustWorker(t, s, id).State)
	_, ok := promptTo(fake.Prompts(), "eoC:p1")
	require.False(t, ok)
}
