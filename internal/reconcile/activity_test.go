package reconcile

// GUIDELINE TESTS — rev7 T3.6 (D9 completion: human-activity back-off).
//
// Pinned surface (on Engine):
//   - ApplyHumanActivity(ctx, paneID string) error — daemon feeds every
//     herdrsock ActivityEvent (focus/scroll) here.
//   - NoteSelfPaneOp(paneID string) — arco's own pane-touching paths (prompt
//     delivery etc.) mark the pane so the resulting activity echo is excluded.
//   - SelfOpWindow time.Duration — how long after a self op activity on that
//     pane is considered arco-caused (0 = implementation default).
//   - ActivityRestoreAfter time.Duration — quiet period after which Sweep
//     restores an activity-demoted session to auto (0 = implementation default,
//     which must be LONG, never instant-restore).
//
// Pinned semantics:
//   - human activity on a pane belonging to an AUTO session demotes that
//     session to assist (the human is present — arco backs off). Assist and
//     manual sessions are never changed by activity.
//   - activity arco itself caused (within SelfOpWindow of NoteSelfPaneOp on
//     that pane) is excluded — no demote.
//   - an unknown pane is silently ignored (operator's own panes flow through
//     the same subscription).
//   - Sweep restores auto ONLY for sessions the back-off itself demoted, and
//     only after ActivityRestoreAfter of quiet. An operator's explicit assist
//     is NEVER auto-promoted back to auto.
//
// Reuses mode_test.go's setMode(t, s, workerID, mode) (actor "operator").

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
)

func workerMode2(t *testing.T, s *ledger.Store, workerID string) core.SupervisionMode {
	t.Helper()
	w, err := s.Reader().GetWorker(workerID)
	require.NoError(t, err)
	sess, err := s.Reader().GetSession(w.OwnerSession)
	require.NoError(t, err)
	return sess.SupervisionMode
}

func TestActivity_HumanFocusDemotesAutoToAssist(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/act1", "base")
	setAgentRef(t, s, id, "wA:p1")
	setMode(t, s, id, core.ModeAuto)

	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:p1"))
	require.Equal(t, core.ModeAssist, workerMode2(t, s, id),
		"human presence on a worker pane demotes auto → assist")
}

func TestActivity_AssistAndManualUnchanged(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/act2", "base")
	setAgentRef(t, s, id, "wA:p2")

	// Default session mode is assist; activity must not touch it.
	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:p2"))
	require.Equal(t, core.ModeAssist, workerMode2(t, s, id))

	setMode(t, s, id, core.ModeManual)
	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:p2"))
	require.Equal(t, core.ModeManual, workerMode2(t, s, id),
		"manual is an operator statement — activity never changes it")
}

func TestActivity_SelfCausedEchoExcluded(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/act3", "base")
	setAgentRef(t, s, id, "wA:p3")
	setMode(t, s, id, core.ModeAuto)

	// arco touched the pane (e.g. prompt delivery); the focus/scroll echo that
	// herdr pushes right after must NOT read as human presence.
	e.NoteSelfPaneOp("wA:p3")
	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:p3"))
	require.Equal(t, core.ModeAuto, workerMode2(t, s, id), "self-caused echo excluded")

	// Past the exclusion window the same signal is human again.
	e.SelfOpWindow = time.Nanosecond
	e.NoteSelfPaneOp("wA:p3")
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:p3"))
	require.Equal(t, core.ModeAssist, workerMode2(t, s, id), "window expired → real activity demotes")
}

func TestActivity_UnknownPaneIgnored(t *testing.T) {
	e, s, _ := newEngine(t)
	id := mkRunning(t, e, s, "/wt/act4", "base")
	setAgentRef(t, s, id, "wA:p4")
	setMode(t, s, id, core.ModeAuto)

	require.NoError(t, e.ApplyHumanActivity(context.Background(), "operator-own-pane"),
		"operator's own panes are silently ignored")
	require.Equal(t, core.ModeAuto, workerMode2(t, s, id))
}

func TestActivity_SweepRestoresAutoAfterQuiet(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/act5", "base")
	setAgentRef(t, s, id, "wA:p5")
	setMode(t, s, id, core.ModeAuto)
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}

	require.NoError(t, e.ApplyHumanActivity(context.Background(), "wA:p5"))
	require.Equal(t, core.ModeAssist, workerMode2(t, s, id))

	// Still within the quiet period → no restore.
	e.ActivityRestoreAfter = time.Hour
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, core.ModeAssist, workerMode2(t, s, id), "quiet period not over — stays assist")

	// Quiet period elapsed → the back-off restores what it demoted.
	e.ActivityRestoreAfter = time.Nanosecond
	time.Sleep(2 * time.Millisecond)
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, core.ModeAuto, workerMode2(t, s, id), "back-off restores its own demotion")
}

func TestActivity_OperatorAssistNeverAutoPromoted(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkRunning(t, e, s, "/wt/act6", "base")
	setAgentRef(t, s, id, "wA:p6")
	setMode(t, s, id, core.ModeAssist) // the OPERATOR chose assist
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + id, Alive: true}}

	e.ActivityRestoreAfter = time.Nanosecond
	time.Sleep(2 * time.Millisecond)
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, core.ModeAssist, workerMode2(t, s, id),
		"only activity-demoted sessions are restored — an operator's assist stands")
}
