package reconcile

// Supplementary T3.3 tests (the pinned guideline surface lives in
// vmroute_test.go): Dispatch/Delegate routing, the per-VM transient-error
// posture (a dropped host observes nothing, never mass-finalizes), registry
// gaps, per-VM GitHeads, orphan-reaper/kill routing, redeliver/diff routing,
// and the local-only herdrsock guard.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// flakyVM wraps a Fake with an injectable ListAgents error — the "one host's
// ssh dropped" shape the per-VM sweep posture is about.
type flakyVM struct {
	*vm.Fake
	ListErr error
}

func (f *flakyVM) ListAgents(ctx context.Context) ([]core.AgentObs, error) {
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	return f.Fake.ListAgents(ctx)
}

func TestVMRoute_DispatchPromptsOnAssignedVM(t *testing.T) {
	e, s, local := newEngine(t)
	remote := vm.NewFake()
	e.VMs = map[string]core.VMClient{"vm1": remote}
	e.DefaultVM = "vm1"

	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)
	require.Len(t, remote.Prompts(), 1, "prompt-path launch routed to the assigned VM")
	require.Empty(t, local.Prompts())
	require.Equal(t, "vm1", mustWorker(t, s, res.WorkerID).VM)
}

func TestVMRoute_DispatchUnknownVMFailsBeforeAnyWrite(t *testing.T) {
	e, s, local := newEngine(t)
	e.VMs = map[string]core.VMClient{}
	e.DefaultVM = "ghost"

	_, err := e.Dispatch(context.Background(), "", "task", true)
	require.ErrorIs(t, err, core.ErrUnknownVM)
	require.Empty(t, local.Prompts())
	ws, err := s.Reader().ListWorkers(core.WorkerFilter{})
	require.NoError(t, err)
	require.Empty(t, ws, "refused pre-intent: no worker row, no session")
}

// One host's ListAgents error must not mass-finalize its workers OR abort the
// sweep for the rest of the fleet: the flaky VM's worker observes nothing (no
// miss accrual), while the local worker is swept normally.
func TestVMRoute_SweepVMErrorObservesNothingForItsWorkers(t *testing.T) {
	e, s, local := newEngine(t)
	remote := &flakyVM{Fake: vm.NewFake(), ListErr: errors.New("ssh: connection reset")}
	e.VMs = map[string]core.VMClient{"vm1": remote}
	e.MissThreshold = 1

	rid := mkRunning(t, e, s, "/wt/vmx1", "base")
	setWorkerVM(t, s, rid, "vm1")
	setAgentRef(t, s, rid, "pane:r")
	lid := mkRunning(t, e, s, "/wt/vmx2", "base")
	setAgentRef(t, s, lid, "pane:l")
	local.Agents = []core.AgentObs{{Ref: "pane:l", Alive: true}}

	for i := 0; i < 3; i++ { // repeatedly: transient noise never accrues misses
		_, err := e.Sweep(context.Background())
		require.NoError(t, err, "a per-VM error is not a sweep error when routing is on")
	}
	require.Equal(t, core.WorkerRunning, mustWorker(t, s, rid).State,
		"unobservable VM → its worker is never finalized")
	require.Equal(t, core.WorkerRunning, mustWorker(t, s, lid).State,
		"the healthy VM's worker is still swept")

	// Host recovers with the agent gone → normal finalize resumes from zero.
	remote.ListErr = nil
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, core.WorkerLost, mustWorker(t, s, rid).State)
}

// A worker on a VM the registry doesn't know is unobservable, not dead: the
// sweep must never finalize on a registry gap (e.g. an entry removed from the
// config while its workers still exist).
func TestVMRoute_SweepRegistryGapNeverFinalizes(t *testing.T) {
	e, s, _ := newEngine(t)
	e.VMs = map[string]core.VMClient{"vm1": vm.NewFake()}
	e.MissThreshold = 1

	id := mkRunning(t, e, s, "/wt/vmx3", "base")
	setWorkerVM(t, s, id, "vm2")

	for i := 0; i < 3; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	require.Equal(t, core.WorkerRunning, mustWorker(t, s, id).State)
}

// GitHeads is per-VM too: a dead remote worker whose HEAD advanced (as read
// from ITS VM) finalizes completed_candidate, not lost.
func TestVMRoute_SweepGitHeadsReadFromOwnVM(t *testing.T) {
	e, s, local := newEngine(t)
	remote := vm.NewFake()
	e.VMs = map[string]core.VMClient{"vm1": remote}
	e.MissThreshold = 1

	id := mkRunning(t, e, s, "/wt/vmx4", "base")
	setWorkerVM(t, s, id, "vm1")
	setAgentRef(t, s, id, "pane:h")
	remote.Heads["/wt/vmx4"] = "advanced"
	local.Heads["/wt/vmx4"] = "base" // a stale local read must not be consulted

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, core.WorkerCompletedCandidate, mustWorker(t, s, id).State,
		"HEAD progress observed via the worker's own VM")
}

// The orphan-agent reaper stops a terminal worker's lingering agent via the
// worker's OWN VM — never another host's client, even for an identical ref.
func TestVMRoute_ReaperRoutesToWorkersVM(t *testing.T) {
	e, s, local := newEngine(t)
	remote := vm.NewFake()
	e.VMs = map[string]core.VMClient{"vm1": remote}

	id := mkRunning(t, e, s, "/wt/vmx5", "base")
	seedTerminalWithAgent(t, s, id, "wV:p1", "term_V")
	setWorkerVM(t, s, id, "vm1")
	remote.Agents = []core.AgentObs{{Ref: "wV:p1", BootID: "term_V", Alive: true}}

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.AgentsReaped)
	require.Contains(t, remote.Killed(), "wV:p1")
	require.Empty(t, local.Killed())
}

// KillWorker on an unresolvable VM still kills durably in the ledger — there is
// simply no client to stop the agent with (best-effort side effect skipped).
func TestVMRoute_KillUnknownVMStillKillsInLedger(t *testing.T) {
	e, s, local := newEngine(t)
	e.VMs = map[string]core.VMClient{}

	id := mkRunning(t, e, s, "/wt/vmx6", "base")
	setWorkerVM(t, s, id, "ghost")
	setAgentRef(t, s, id, "pane:g")

	require.NoError(t, e.KillWorker(context.Background(), id))
	require.Equal(t, core.WorkerKilled, mustWorker(t, s, id).State)
	require.Empty(t, local.Killed(), "no fallback kill on the local client")
}

func TestVMRoute_RedeliverRoutesToWorkersVM(t *testing.T) {
	e, s, local := newEngine(t)
	remote := vm.NewFake()
	e.VMs = map[string]core.VMClient{"vm1": remote}

	id := mkRunning(t, e, s, "/wt/vmx7", "base")
	setWorkerVM(t, s, id, "vm1")
	setAgentRef(t, s, id, "pane:rd")
	before := len(local.Prompts()) // mkRunning's dispatch prompted the local client

	require.NoError(t, e.RedeliverInitialTask(context.Background(), id))
	require.Len(t, remote.Prompts(), 1, "redelivery lands on the worker's VM")
	require.Len(t, local.Prompts(), before, "no redelivery on any other VM")
}

func TestVMRoute_WorkerDiffUnknownVMErrs(t *testing.T) {
	e, s, _ := newEngine(t)
	e.VMs = map[string]core.VMClient{}

	id := mkRunning(t, e, s, "/wt/vmx8", "base")
	setWorkerVM(t, s, id, "ghost")

	_, err := e.WorkerDiff(context.Background(), id)
	require.ErrorIs(t, err, core.ErrUnknownVM)
}

// The herdrsock push subscription is LOCAL-ONLY: with routing on, a local pane
// frame whose id collides with a REMOTE worker's per-host ref is someone else's
// pane — it must not transition (or back off) that worker's session.
func TestVMRoute_LocalHerdrPushIgnoresRemoteWorkers(t *testing.T) {
	e, s, _ := newEngine(t)
	e.VMs = map[string]core.VMClient{"vm1": vm.NewFake()}

	id := mkRunning(t, e, s, "/wt/vmx9", "base")
	setWorkerVM(t, s, id, "vm1")
	setAgentRef(t, s, id, "w9:p1")

	require.NoError(t, e.ApplyHerdrStatus(context.Background(), "w9:p1", "done"))
	require.Equal(t, core.WorkerRunning, mustWorker(t, s, id).State,
		"a colliding local pane id must not finalize a remote worker")

	setMode(t, s, id, core.ModeAuto)
	require.NoError(t, e.ApplyHumanActivity(context.Background(), "w9:p1"))
	require.Equal(t, core.ModeAuto, workerMode2(t, s, id),
		"local pane activity must not back off a remote worker's session")
}
