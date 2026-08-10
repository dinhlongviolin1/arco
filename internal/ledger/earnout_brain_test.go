package ledger

// rev7 T3.5 supplements to the guideline tests: the BRAIN answer path
// (AnswerQuestionBrain) — attribution, no grant, no tally, and its guards.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func brainAnswer(s *Store, escID, text string) error {
	return s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AnswerQuestionBrain(escID, text, core.Event{
			Kind: "auto_answer", Actor: "brain", Payload: `{"decided_by":"brain"}`,
		})
	})
}

func TestAnswerQuestionBrain_StampsBrainResumesAndFeedsNoTally(t *testing.T) {
	s := newTestStore(t)
	escID := draftedEsc(t, s, "question", "clarify", "use foo=bar")
	require.NoError(t, brainAnswer(s, escID, "use foo=bar"))

	esc, err := s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.Equal(t, "answered", esc.Status)
	require.Equal(t, "brain", esc.AnsweredBy)
	require.Equal(t, "brain", esc.DecidedBy)
	require.Equal(t, "once", esc.OnceOrAlways, "a brain answer is scope-once by construction")
	require.Equal(t, "use foo=bar", esc.AnswerText)

	w, err := s.Reader().GetWorker(esc.WorkerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w.State, "the brain answer resumes the worker")

	a, tot := agreement(t, s, "clarify")
	require.Zero(t, a)
	require.Zero(t, tot, "a brain answer must not ratify itself")

	var grants int
	require.NoError(t, s.DB().QueryRow(`SELECT COUNT(1) FROM grants`).Scan(&grants))
	require.Zero(t, grants, "a brain answer can never promote a grant")

	// Idempotence guard: an already-answered escalation is wrong-state.
	require.ErrorIs(t, brainAnswer(s, escID, "use foo=bar"), core.ErrEscalationState)
}

func TestAnswerQuestionBrain_RefusesConfirmsAndEmptyDrafts(t *testing.T) {
	s := newTestStore(t)

	// A confirm is out of reach whatever the text (ports.go grant rule).
	confirm := draftedEsc(t, s, "confirm", "proceed-confirmation", "safe to proceed")
	require.ErrorIs(t, brainAnswer(s, confirm, "safe to proceed"), core.ErrEscalationState)

	// An undrafted question has nothing earned to say — fail closed.
	undrafted := draftedEsc(t, s, "question", "clarify", "")
	require.Error(t, brainAnswer(s, undrafted, "anything"))
	esc, err := s.Reader().GetEscalation(undrafted)
	require.NoError(t, err)
	require.Equal(t, "pending", esc.Status)
}

func TestAnswerQuestionBrain_NeverDrivesPoolOwnedWorker(t *testing.T) {
	s := newTestStore(t)
	escID := draftedEsc(t, s, "question", "clarify", "go with plan A")
	esc, err := s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.ReleaseWorker(esc.WorkerID, "operator")
	}))

	// Same MED-4 posture as decide(): the answer is recorded, the pool-owned
	// worker is NOT driven back to running under the pool sentinel.
	require.NoError(t, brainAnswer(s, escID, "go with plan A"))
	esc, err = s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.Equal(t, "answered", esc.Status)
	require.Empty(t, esc.ResumedAt)
	w, err := s.Reader().GetWorker(esc.WorkerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerWaitingForUser, w.State)
}
