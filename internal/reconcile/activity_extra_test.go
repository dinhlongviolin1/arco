package reconcile

// Tests beyond the pinned guideline set (activity_test.go): quiet-period
// extension, restore bookkeeping, defaults, and the self-op marking done by the
// real pane-touching paths.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
)

func sessionOf(t *testing.T, s *ledger.Store, workerID string) string {
	t.Helper()
	w, err := s.Reader().GetWorker(workerID)
	require.NoError(t, err)
	return w.OwnerSession
}

// Repeat activity on an already-demoted session extends the quiet period, so a
// human who keeps working never gets autonomy restored under their hands.
func TestActivity_RepeatActivityExtendsQuietPeriod(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/actx1", "base")
	setAgentRef(t, s, id, "wA:x1")
	setMode(t, s, id, core.ModeAuto)
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}

	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:x1"))
	require.Equal(t, core.ModeAssist, workerMode2(t, s, id))

	// A second event on the assist session re-stamps the timer without a demote.
	e.ActivityRestoreAfter = 500 * time.Millisecond
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:x1"))
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, core.ModeAssist, workerMode2(t, s, id), "fresh activity keeps the session in assist")
}

// Once restored, the demotion is forgotten: a later operator-set assist is not
// promoted back to auto by a subsequent sweep.
func TestActivity_RestoreIsOneShot(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/actx2", "base")
	setAgentRef(t, s, id, "wA:x2")
	setMode(t, s, id, core.ModeAuto)
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}

	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:x2"))
	e.ActivityRestoreAfter = time.Nanosecond
	time.Sleep(2 * time.Millisecond)
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.ActivityRestored)
	require.Equal(t, core.ModeAuto, workerMode2(t, s, id))

	setMode(t, s, id, core.ModeAssist) // the operator's own choice, after the restore
	res, err = e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.ActivityRestored)
	require.Equal(t, core.ModeAssist, workerMode2(t, s, id))
}

// An operator who moves an activity-demoted session to manual outranks the
// pending restore — the back-off must not undo a manual statement.
func TestActivity_ManualBeatsPendingRestore(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/actx3", "base")
	setAgentRef(t, s, id, "wA:x3")
	setMode(t, s, id, core.ModeAuto)
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}

	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:x3"))
	setMode(t, s, id, core.ModeManual)

	e.ActivityRestoreAfter = time.Nanosecond
	time.Sleep(2 * time.Millisecond)
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, core.ModeManual, workerMode2(t, s, id))
}

// The zero-value restore window is LONG (never an instant re-promotion), and a
// demoted session stays assist across an ordinary sweep.
func TestActivity_DefaultRestoreWindowIsLong(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/actx4", "base")
	setAgentRef(t, s, id, "wA:x4")
	setMode(t, s, id, core.ModeAuto)
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}

	require.GreaterOrEqual(t, e.activityRestoreAfter(), 15*time.Minute)
	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:x4"))
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, res.ActivityRestored)
	require.Equal(t, core.ModeAssist, workerMode2(t, s, id))
}

// The demote is attributed to the back-off, not the operator — the ledger must
// show who changed the mode (D9).
func TestActivity_DemoteIsAttributedInTheLedger(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/actx5", "base")
	setAgentRef(t, s, id, "wA:x5")
	setMode(t, s, id, core.ModeAuto)

	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:x5"))
	sid := sessionOf(t, s, id)
	evs, err := s.Reader().EventsSince(0, 500)
	require.NoError(t, err)
	var found bool
	for _, ev := range evs {
		if ev.Kind == "mode_change" && ev.SessionID == sid && ev.Actor == activityActor {
			found = true
		}
	}
	require.True(t, found, "the demote is auditable as an activity-backoff mode_change")
}

// The real delivery path marks its pane, so the prompt's own echo can't demote
// the session that just got auto-prompted.
func TestActivity_DeliverInitialTaskMarksSelfOp(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/actx6", "base")
	setAgentRef(t, s, id, "wA:x6")
	setMode(t, s, id, core.ModeAuto)

	e.deliverInitialTask(context.Background(), id, sessionOf(t, s, id), "wA:x6", "task")
	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:x6"))
	require.Equal(t, core.ModeAuto, workerMode2(t, s, id))
}

// Degenerate inputs are inert: no pane, and a self-op note for a pane no worker
// owns, must never panic or write.
func TestActivity_EmptyPaneAndUnknownSelfOpAreInert(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/actx7", "base")
	setAgentRef(t, s, id, "wA:x7")
	setMode(t, s, id, core.ModeAuto)

	e.NoteSelfPaneOp("")
	e.NoteSelfPaneOp("nobody:p0")
	require.NoError(t, e.ApplyHumanActivity(context.Background(), ""))
	require.Equal(t, core.ModeAuto, workerMode2(t, s, id))
}
