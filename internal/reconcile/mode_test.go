package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// setMode flips the supervision mode of the session owning the given worker.
func setMode(t *testing.T, s *ledger.Store, workerID string, m core.SupervisionMode) {
	t.Helper()
	w, err := s.Reader().GetWorker(workerID)
	require.NoError(t, err)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetSessionMode(w.OwnerSession, m, "operator")
	}))
}

// D9 mode matrix: which AUTONOMOUS arco actions are allowed per session mode.
// Operator-initiated actions (CLI kill/redeliver/answer/spawn/verify) are NEVER
// gated — the mode restricts arco's autonomy, not the human's power.
func TestModeMatrix(t *testing.T) {
	cases := []struct {
		mode   core.SupervisionMode
		action core.AutonomousAction
		want   bool
	}{
		// manual: observe + ledger only — arco never calls the brain, never
		// touches the world, never pings the phone.
		{core.ModeManual, core.ActBrainDraft, false},
		{core.ModeManual, core.ActBrainAct, false},
		{core.ModeManual, core.ActNotify, false},
		{core.ModeManual, core.ActReapAgent, false},

		// assist (default): notify + draft, never auto-act.
		{core.ModeAssist, core.ActBrainDraft, true},
		{core.ModeAssist, core.ActBrainAct, false},
		{core.ModeAssist, core.ActNotify, true},
		{core.ModeAssist, core.ActReapAgent, true},

		// auto: full autonomy (earn-out gating on top is T3.5).
		{core.ModeAuto, core.ActBrainDraft, true},
		{core.ModeAuto, core.ActBrainAct, true},
		{core.ModeAuto, core.ActNotify, true},
		{core.ModeAuto, core.ActReapAgent, true},
	}
	for _, c := range cases {
		require.Equal(t, c.want, c.mode.Allows(c.action),
			"mode=%s action=%v", c.mode, c.action)
	}
}

// manual: the ledger keeps observing (escalation row opens) but no card is pushed.
func TestManualMode_EscalationOpened_NoNotify(t *testing.T) {
	e, s, _ := newEngine(t)
	rec := &notify.Recorder{}
	e.Notify = rec
	id := mkRunning(t, e, s, "/wt/m1", "base")
	setMode(t, s, id, core.ModeManual)

	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: id, Alive: true, WaitingInput: true,
	}))

	pend, err := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: id})
	require.NoError(t, err)
	require.Len(t, pend, 1, "manual still records the question in the ledger")
	require.Empty(t, rec.Cards(), "manual must not push notifications")
}

// assist still pushes the decision card (control case for the manual test).
func TestAssistMode_EscalationOpened_Notifies(t *testing.T) {
	e, s, _ := newEngine(t)
	rec := &notify.Recorder{}
	e.Notify = rec
	id := mkRunning(t, e, s, "/wt/m2", "base")
	// default mode is assist — no setMode on purpose

	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: id, Alive: true, WaitingInput: true,
	}))
	waitCards(t, rec, 1)
}

// assist: a brain ACTING step (run_again) must NOT prompt the agent; it degrades
// to a question escalation carrying the instruction as the draft.
func TestAssistMode_BrainRunAgain_DegradesToEscalation(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/m3", "base")
	before := len(fake.Prompts())

	e.applyStep(context.Background(), id, "cid-assist-1", core.StepResult{
		Kind: "run_again", Instruction: "keep going with step 2", Reason: "plan says so",
	})

	require.Len(t, fake.Prompts(), before, "assist must never prompt the agent on its own")
	pend, err := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: id})
	require.NoError(t, err)
	require.Len(t, pend, 1, "the acting step degrades to an escalation")
	require.Equal(t, "keep going with step 2", pend[0].DraftAnswer,
		"the instruction rides as the draft so the operator can one-tap approve")
}

// assist: a brain dispatch (delegate) must NOT spawn a child worker.
func TestAssistMode_BrainDispatch_DoesNotSpawn(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/m4", "base")
	before := len(fake.Prompts())

	e.applyStep(context.Background(), id, "cid-assist-2", core.StepResult{
		Kind: "dispatch", Instruction: "extract the parser into a subtask", Reason: "parallelize",
	})

	require.Len(t, fake.Prompts(), before, "no child agent may be launched in assist")
	workers, err := s.Reader().ListWorkers(core.WorkerFilter{})
	require.NoError(t, err)
	require.Len(t, workers, 1, "no child worker row in assist")
}

// auto: the same acting step prompts the agent (existing behavior preserved).
func TestAutoMode_BrainRunAgain_Prompts(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/m5", "base")
	setMode(t, s, id, core.ModeAuto)
	before := len(fake.Prompts())

	e.applyStep(context.Background(), id, "cid-auto-1", core.StepResult{
		Kind: "run_again", Instruction: "keep going", Reason: "plan",
	})

	require.Len(t, fake.Prompts(), before+1, "auto acts: the agent is prompted")
}

// manual: brainClassify is a no-op — no brain call is even attempted.
func TestManualMode_BrainClassify_Skipped(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/m6", "base")
	setMode(t, s, id, core.ModeManual)

	e.Brain.Enabled = true // enabled, but manual must still refuse to invoke it
	e.brainClassify(context.Background(), id)

	evs, err := s.Reader().RecentWorkerEvents(id, 100)
	require.NoError(t, err)
	for _, ev := range evs {
		require.NotEqual(t, "brain_intent", ev.Kind, "manual must not open a brain intent")
	}
}

// D9 "who answered": the human answer path stamps actor operator on its event.
func TestAnswerQuestion_RecordsOperatorActor(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/m7", "base")
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: id, Alive: true, WaitingInput: true,
	}))
	pend, err := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: id})
	require.NoError(t, err)
	require.Len(t, pend, 1)

	require.NoError(t, e.AnswerQuestion(context.Background(), pend[0].ID, "go with plan B", core.ScopeOnce))

	evs, err := s.Reader().RecentWorkerEvents(id, 100)
	require.NoError(t, err)
	var found bool
	for _, ev := range evs {
		if ev.Kind == "question_esc" {
			found = true
			require.Equal(t, "operator", ev.Actor, "the answer event must record WHO answered")
		}
	}
	require.True(t, found, "question_esc event recorded")
}
