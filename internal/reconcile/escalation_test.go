package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// promptTo returns the last Fake prompt delivered to a given pane/workspace target.
func promptTo(prompts []vm.Prompted, target string) (vm.Prompted, bool) {
	for i := len(prompts) - 1; i >= 0; i-- {
		if prompts[i].Workspace == target {
			return prompts[i], true
		}
	}
	return vm.Prompted{}, false
}

// parkWaiting binds a pane to a running worker, parks it waiting_for_user, and
// opens a pending question escalation; returns the escalation id.
func parkWaiting(t *testing.T, s *ledger.Store, id, ref string) string {
	t.Helper()
	var escID string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.BindLaunch(id, "/wt/ans", "base", ref, "term_"+ref); err != nil {
			return err
		}
		w, _ := tx.GetWorker(id)
		if err := tx.TransitionWorker(id, core.WorkerWaitingForUser, w.Rev, core.Event{
			Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"}); err != nil {
			return err
		}
		var e2 error
		escID, e2 = tx.OpenEscalation(core.Escalation{WorkerID: id, SessionID: w.OwnerSession, Kind: "question", Action: "q?"})
		return e2
	}))
	return escID
}

// MED-2 (audit): answering a question resumes the worker AND delivers the answer
// text to its agent's pane — the ledger resume alone would leave the agent parked.
func TestAnswerQuestion_DeliversAnswerToResumedAgent(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/ans", "base")
	escID := parkWaiting(t, s, id, "wZ:p1")

	require.NoError(t, e.AnswerQuestion(context.Background(), escID, "use foo=bar", core.ScopeOnce))
	e.Exec.Wait() // drain the async delivery

	require.Equal(t, core.WorkerRunning, mustWorker(t, s, id).State, "answer resumes the worker")
	p, ok := promptTo(fake.Prompts(), "wZ:p1")
	require.True(t, ok, "answer delivered to the captured pane (AgentRef)")
	require.Contains(t, p.Text, "use foo=bar", "the human's answer text reaches the agent")
}

// A pool-owned worker is NOT resumed by an answer (MED-4), so nothing is delivered.
func TestAnswerQuestion_PoolWorkerNoResumeNoDelivery(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/ans", "base")
	escID := parkWaiting(t, s, id, "wY:p1")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.ReleaseWorker(id, "cli") }))

	require.NoError(t, e.AnswerQuestion(context.Background(), escID, "answer", core.ScopeOnce))
	e.Exec.Wait()

	require.Equal(t, core.WorkerWaitingForUser, mustWorker(t, s, id).State, "pool worker not resumed (MED-4)")
	_, ok := promptTo(fake.Prompts(), "wY:p1")
	require.False(t, ok, "no delivery to a worker the answer didn't resume")
}

// An approved confirm delivers a continue signal; a rejected confirm blocks the
// worker and delivers nothing (it must not proceed).
func TestDecideConfirm_ApproveDelivers_RejectBlocks(t *testing.T) {
	e, s, fake := newEngine(t)
	// approve
	idA := mkRunning(t, e, s, "/wt/ans", "base")
	escA := parkWaitingConfirm(t, s, idA, "wA:p1")
	require.NoError(t, e.DecideConfirm(context.Background(), escA, true, core.ScopeOnce))
	e.Exec.Wait()
	require.Equal(t, core.WorkerRunning, mustWorker(t, s, idA).State)
	_, ok := promptTo(fake.Prompts(), "wA:p1")
	require.True(t, ok, "approval delivered to the agent")

	// reject
	idR := mkRunning(t, e, s, "/wt/ans", "base")
	escR := parkWaitingConfirm(t, s, idR, "wB:p1")
	require.NoError(t, e.DecideConfirm(context.Background(), escR, false, core.ScopeOnce))
	e.Exec.Wait()
	require.Equal(t, core.WorkerBlocked, mustWorker(t, s, idR).State, "rejection blocks the worker")
	_, ok = promptTo(fake.Prompts(), "wB:p1")
	require.False(t, ok, "a rejected confirm delivers nothing")
}

func parkWaitingConfirm(t *testing.T, s *ledger.Store, id, ref string) string {
	t.Helper()
	var escID string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.BindLaunch(id, "/wt/ans", "base", ref, "term_"+ref); err != nil {
			return err
		}
		w, _ := tx.GetWorker(id)
		if err := tx.TransitionWorker(id, core.WorkerWaitingConfirmation, w.Rev, core.Event{
			Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"}); err != nil {
			return err
		}
		var e2 error
		escID, e2 = tx.OpenEscalation(core.Escalation{WorkerID: id, SessionID: w.OwnerSession, Kind: "confirm", Action: "ok?"})
		return e2
	}))
	return escID
}
