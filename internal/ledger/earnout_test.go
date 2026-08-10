package ledger

// GUIDELINE TESTS — rev7 T3.5 (autonomy earn-out: draft-agreement bookkeeping).
//
// Pinned surface (core.Reader):
//   DraftAgreement(questionClass string) (agree, total int, err error)
//     — per question_class: of the HUMAN decisions taken on escalations that
//     carried a brain DraftAnswer, how many matched the draft. The tally is
//     LEDGER-BACKED: a daemon restart must not reset it.
//
// Pinned semantics (counted when the human decides):
//   - Only decisions on escalations with a NON-EMPTY draft count; an undrafted
//     escalation never moves any tally.
//   - question: agree iff the human's answer equals the draft modulo
//     leading/trailing whitespace and ASCII case.
//   - confirm: the draft is the brain's case to proceed — an approval agrees,
//     a rejection disagrees.
//   - Tallies are keyed by the escalation's question_class.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// draftedEsc parks a fresh worker in the right waiting state and opens a
// pending escalation of the given kind/class carrying the brain's draft.
func draftedEsc(t *testing.T, s *Store, kind, class, draft string) string {
	t.Helper()
	session := newWork(t, s)
	worker := newWorker(t, s, session)
	waiting := core.WorkerWaitingForUser
	if kind == "confirm" {
		waiting = core.WorkerWaitingConfirmation
	}
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.TransitionWorker(worker, core.WorkerRunning, 0,
			core.Event{Kind: "state_change", WorkerID: worker})
	}))
	var escID string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, err := tx.GetWorker(worker)
		if err != nil {
			return err
		}
		if err := tx.TransitionWorker(worker, waiting, w.Rev,
			core.Event{Kind: "state_change", WorkerID: worker}); err != nil {
			return err
		}
		var e2 error
		escID, e2 = tx.OpenEscalation(core.Escalation{
			WorkerID: worker, SessionID: session, Kind: kind,
			QuestionClass: class, Action: "step?", DraftAnswer: draft,
		})
		return e2
	}))
	return escID
}

func humanAnswer(t *testing.T, s *Store, escID, text string) {
	t.Helper()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AnswerQuestion(escID, text, core.ScopeOnce,
			core.Event{Kind: "question_ans", Payload: `{"decided_by":"human"}`})
	}))
}

func humanConfirm(t *testing.T, s *Store, escID string, yes bool) {
	t.Helper()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.DecideConfirm(escID, yes, core.ScopeOnce,
			core.Event{Kind: "confirm_dec", Payload: `{"decided_by":"human"}`})
	}))
}

func agreement(t *testing.T, s *Store, class string) (agree, total int) {
	t.Helper()
	agree, total, err := s.Reader().DraftAgreement(class)
	require.NoError(t, err)
	return agree, total
}

func TestDraftAgreement_QuestionMatchModuloSpaceAndCase(t *testing.T) {
	s := newTestStore(t)

	a, tot := agreement(t, s, "clarify")
	require.Zero(t, a)
	require.Zero(t, tot, "an untouched class reads (0,0), not an error")

	// The human sends the draft back with different surrounding whitespace and
	// ASCII case — that is the SAME decision, so it agrees.
	humanAnswer(t, s, draftedEsc(t, s, "question", "clarify", "use foo=bar"), "  Use FOO=bar\n")
	a, tot = agreement(t, s, "clarify")
	require.Equal(t, 1, a)
	require.Equal(t, 1, tot)

	// A materially different answer is a disagreement.
	humanAnswer(t, s, draftedEsc(t, s, "question", "clarify", "pick plan A"), "pick plan B")
	a, tot = agreement(t, s, "clarify")
	require.Equal(t, 1, a, "a different answer must not count as agreement")
	require.Equal(t, 2, tot)
}

func TestDraftAgreement_ConfirmApprovalAgreesRejectionDisagrees(t *testing.T) {
	s := newTestStore(t)

	humanConfirm(t, s, draftedEsc(t, s, "confirm", "proceed-confirmation", "safe: the branch is backed up"), true)
	a, tot := agreement(t, s, "proceed-confirmation")
	require.Equal(t, 1, a, "approving a drafted confirm ratifies the brain's call")
	require.Equal(t, 1, tot)

	humanConfirm(t, s, draftedEsc(t, s, "confirm", "proceed-confirmation", "safe: lockfile is stale"), false)
	a, tot = agreement(t, s, "proceed-confirmation")
	require.Equal(t, 1, a, "a rejection is a disagreement")
	require.Equal(t, 2, tot)
}

func TestDraftAgreement_OnlyDraftedDecisionsCount_PerClass(t *testing.T) {
	s := newTestStore(t)

	// No draft → decided, but never tallied.
	humanAnswer(t, s, draftedEsc(t, s, "question", "clarify", ""), "whatever you think")
	a, tot := agreement(t, s, "clarify")
	require.Zero(t, a)
	require.Zero(t, tot, "an undrafted decision must not feed the tally")

	// Class isolation: a scope-change agreement never leaks into clarify.
	humanAnswer(t, s, draftedEsc(t, s, "question", "scope-change", "yes, split the PR"), "yes, split the PR")
	a, tot = agreement(t, s, "scope-change")
	require.Equal(t, 1, a)
	require.Equal(t, 1, tot)
	a, tot = agreement(t, s, "clarify")
	require.Zero(t, a)
	require.Zero(t, tot, "tallies are keyed by question_class")
}

func TestDraftAgreement_SurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "earnout.db")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	humanAnswer(t, s, draftedEsc(t, s, "question", "resource", "cap workers at 4"), "cap workers at 4")
	require.NoError(t, s.Close())

	s2, err := Open(path)
	require.NoError(t, err)
	defer s2.Close()
	require.NoError(t, s2.Migrate(context.Background()))
	agree, total, err := s2.Reader().DraftAgreement("resource")
	require.NoError(t, err)
	require.Equal(t, 1, agree, "the tally is ledger-backed, not in-memory")
	require.Equal(t, 1, total)
}
