package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// A live herdr agent arco never launched shows up in a scan as UNtracked; after
// Adopt it is a monitor-only worker in a manual-mode session, and the same agent
// scans as tracked (so /adopt all is idempotent).
func TestScanAndAdopt_TracksUnmanagedAgent(t *testing.T) {
	e, s, fake := newEngine(t)
	fake.Agents = []core.AgentObs{{
		Ref: "w5:p1", Workspace: "w5", BootID: "term_sysadmin", State: "idle", Alive: true,
		Kind: "claude", Cwd: "/home/op/sysadmin", Title: "Pull latest", SessionID: "0d64f4be",
	}}

	scan, err := e.ScanAgents(context.Background())
	require.NoError(t, err)
	require.Len(t, scan, 1)
	require.False(t, scan[0].Tracked, "an agent arco didn't launch is untracked")
	require.Equal(t, "w5:p1", scan[0].Ref)
	require.Equal(t, "claude", scan[0].Kind)

	res, err := e.Adopt(context.Background(), "w5:p1")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)

	w, err := s.Reader().GetWorker(res.WorkerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w.State)
	require.Equal(t, "w5:p1", w.AgentRef, "adopted worker correlates on the herdr pane")
	require.Equal(t, "term_sysadmin", w.BootID)
	require.Equal(t, "w5", w.Workspace)
	require.Equal(t, "adopt", w.RunReason)
	require.Empty(t, w.Worktree, "adopted workers are uncontained — no worktree")

	sess, err := s.Reader().GetSession(res.SessionID)
	require.NoError(t, err)
	require.Equal(t, core.ModeManual, sess.SupervisionMode, "adopted sessions are observe-only")

	// scanning again marks the same agent tracked
	scan2, err := e.ScanAgents(context.Background())
	require.NoError(t, err)
	require.Len(t, scan2, 1)
	require.True(t, scan2[0].Tracked)
	require.Equal(t, res.WorkerID, scan2[0].WorkerID)

	// re-adopt refuses (already tracked), returning the existing worker id
	_, err = e.Adopt(context.Background(), "w5:p1")
	require.Error(t, err)
}

func TestAdopt_UnknownRef(t *testing.T) {
	e, _, _ := newEngine(t)
	_, err := e.Adopt(context.Background(), "nope:p9")
	require.Error(t, err)
}

// A dead (terminal-status) agent is not offered by scan — nothing to monitor.
func TestScan_SkipsDeadAgents(t *testing.T) {
	e, _, fake := newEngine(t)
	fake.Agents = []core.AgentObs{{Ref: "w9:p1", Workspace: "w9", State: "done", Alive: false}}
	scan, err := e.ScanAgents(context.Background())
	require.NoError(t, err)
	require.Empty(t, scan)
}
