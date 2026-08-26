package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// An explicit operator mode-set must END the activity back-off's in-memory claim,
// so restoreActivityBackoff can never later promote the operator's standing
// assist back to auto (D9 safety).
func TestSetSessionMode_ClearsActivityDemotion(t *testing.T) {
	e, s, _ := newEngine(t)
	ctx := context.Background()
	sid := ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: sid, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))

	// Simulate the back-off having demoted this session.
	e.mu.Lock()
	if e.activityDemoted == nil {
		e.activityDemoted = map[string]time.Time{}
	}
	e.activityDemoted[sid] = e.Store.Now()
	e.mu.Unlock()

	got, err := e.SetSessionMode(ctx, sid, core.ModeAssist, "operator")
	require.NoError(t, err)
	require.Equal(t, sid, got)

	e.mu.Lock()
	_, present := e.activityDemoted[sid]
	e.mu.Unlock()
	require.False(t, present, "operator mode-set clears the back-off claim")

	sess, err := s.Reader().GetSession(sid)
	require.NoError(t, err)
	require.Equal(t, core.ModeAssist, sess.SupervisionMode)
}

// pruneMisses drops counters for workers no longer liveness-tracked, bounding the
// map; a still-tracked worker keeps its count.
func TestPruneMisses_DropsUntrackedWorkers(t *testing.T) {
	e, _, _ := newEngine(t)
	e.mu.Lock()
	e.misses = map[string]int{"live1": 2, "gone1": 1, "gone2": 3}
	e.mu.Unlock()

	e.pruneMisses(map[string]bool{"live1": true})

	e.mu.Lock()
	defer e.mu.Unlock()
	require.Equal(t, map[string]int{"live1": 2}, e.misses)
}

// With a VM registry, herdr pane/workspace ids are per-host, so scan correlation
// must be VM-scoped: the same ref on VM-b is NOT the worker tracked on VM-a.
func TestScanAgents_CorrelatesPerVMWhenRouting(t *testing.T) {
	e, s, _ := newEngine(t)
	ctx := context.Background()
	vmA, vmB := vm.NewFake(), vm.NewFake()
	// Same pane id "wX:p1" live on BOTH hosts.
	vmA.Agents = []core.AgentObs{{Ref: "wX:p1", Workspace: "wX", Alive: true, Kind: "claude"}}
	vmB.Agents = []core.AgentObs{{Ref: "wX:p1", Workspace: "wX", Alive: true, Kind: "claude"}}
	e.VMs = map[string]core.VMClient{"a": vmA, "b": vmB}

	// A worker arco launched on VM-a with that ref.
	sid := ulid.Make().String()
	wid := ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sid, Status: core.SessionActive, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		return tx.CreateWorker(core.Worker{
			ID: wid, OwnerSession: sid, State: core.WorkerRunning, VM: "a", Workspace: "wX", AgentRef: "wX:p1",
		})
	}))

	scan, err := e.ScanAgents(ctx)
	require.NoError(t, err)
	require.Len(t, scan, 2)
	byVM := map[string]ScannedAgent{}
	for _, a := range scan {
		byVM[a.VM] = a
	}
	require.True(t, byVM["a"].Tracked, "VM-a's agent is the tracked worker")
	require.Equal(t, wid, byVM["a"].WorkerID)
	require.False(t, byVM["b"].Tracked, "the same ref on VM-b is a DIFFERENT, untracked agent")

	// Adopt by the colliding bare ref is refused as ambiguous across VMs.
	_, err = e.Adopt(ctx, "wX:p1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "across VMs")
}
