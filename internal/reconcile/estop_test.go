package reconcile

// Emergency stop (estop): the ESTOP sentinel's EXISTENCE pauses admission and
// every autonomous action, never touching in-flight work. Modeled on the
// pause-new-work/never-kill posture; the file works even with a wedged daemon.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func engage(t *testing.T, e *Engine) string {
	t.Helper()
	e.EStopPath = filepath.Join(t.TempDir(), "ESTOP")
	require.NoError(t, os.WriteFile(e.EStopPath, nil, 0o600)) // even an EMPTY sentinel engages
	return e.EStopPath
}

func TestEStop_RefusesAdmission(t *testing.T) {
	e, _, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	repo, _ := localRepo(t)
	path := engage(t, e)

	_, err := e.Dispatch(context.Background(), "", "task", true)
	require.ErrorIs(t, err, core.ErrPaused)
	_, err = e.Spawn(context.Background(), "", "task", true, repo, "")
	require.ErrorIs(t, err, core.ErrPaused)
	require.Empty(t, fake.Launched(), "nothing may launch while paused")

	// Removing the sentinel releases the stop — same engine, no restart needed.
	require.NoError(t, os.Remove(path))
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)
}

func TestEStop_SweepStandsDownAutonomyButKeepsObserving(t *testing.T) {
	// Earned-out drafted question, every promotion gate passing — but paused:
	// the sweep must leave it for the human. Liveness bookkeeping still runs.
	e, s, fake := newEarnedOut(t)
	id := mkRunning(t, e, s, "/wt/estop", "base")
	setMode(t, s, id, core.ModeAuto)
	escID := parkDrafted(t, s, id, "es:p1", "question", "clarify", "the draft")
	engage(t, e)

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	e.Exec.Wait()

	esc, err := s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.Equal(t, "pending", esc.Status, "no auto-answers under an engaged estop")
	require.Zero(t, res.AutoAnswered)
	require.Positive(t, res.Observed, "liveness observation keeps running while paused")
	_, ok := promptTo(fake.Prompts(), "es:p1")
	require.False(t, ok)

	// Operator-initiated actions are NOT paused: the human can still answer.
	require.NoError(t, e.AnswerQuestion(context.Background(), escID, "the draft", core.ScopeOnce))
}

func TestEStop_UnsetPathNeverPauses(t *testing.T) {
	e, _, _ := newEngine(t)
	require.False(t, e.Paused(), "tests/embedded use without a sentinel path run normally")
}
