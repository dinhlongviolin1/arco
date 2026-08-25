package reconcile

// GUIDELINE TESTS — rev7 T3.3 (cross-VM wiring: route VM ops per worker's VM).
//
// Pinned seam (on Engine):
//   - VMs map[string]core.VMClient — the named-VM registry the daemon builds
//     from the configured VM fleet (vm.NewRemote per host). nil = routing OFF:
//     today's single-client behavior, VM names stay pure labels. Non-nil =
//     routing ON: a worker's VM name MUST resolve (its entry, or e.VM for "");
//     a named VM with no entry never silently falls back to the local client.
//
// Pinned semantics:
//   - Spawn: the launch (and everything after it) goes to the ASSIGNED VM's
//     client. With routing on, spawning onto an unknown VM name fails BEFORE
//     any launch anywhere — a typo'd VM must not land a worker on the wrong
//     machine.
//   - Sweep liveness: a worker is correlated ONLY against ITS OWN VM's
//     observations. Alive on its own VM keeps it alive even when every other
//     VM reports nothing; an agent with the SAME ref on a DIFFERENT VM is not
//     this worker's agent (pane ids are per-host and can collide).
//   - KillWorker: the agent stop goes to the worker's VM, never another.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// setWorkerVM labels an existing worker as living on a named VM (the dispatch
// path stamps DefaultVM; tests retarget directly like sweep_test's worktree
// patch).
func setWorkerVM(t *testing.T, s *ledger.Store, workerID, vmName string) {
	t.Helper()
	_, err := s.DB().Exec(`UPDATE workers SET vm=? WHERE id=?`, vmName, workerID)
	require.NoError(t, err)
}

func TestVMRoute_SpawnLaunchesOnAssignedVM(t *testing.T) {
	e, s, local := newEngine(t)
	remote := vm.NewFake()
	e.VMs = map[string]core.VMClient{"vm1": remote}
	e.DefaultVM = "vm1"
	e.ConfigDir = t.TempDir()
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "", "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)

	require.Len(t, remote.Launched(), 1, "launch routed to the assigned VM's client")
	require.Empty(t, local.Launched(), "nothing launched on the default/local client")

	w, err := s.Reader().GetWorker(res.WorkerID)
	require.NoError(t, err)
	require.Equal(t, "vm1", w.VM)
}

func TestVMRoute_SpawnUnknownVMFailsBeforeLaunch(t *testing.T) {
	e, _, local := newEngine(t)
	e.VMs = map[string]core.VMClient{} // routing ON, fleet has no "ghost"
	e.DefaultVM = "ghost"
	e.ConfigDir = t.TempDir()
	repo, _ := localRepo(t)

	_, err := e.Spawn(context.Background(), "", "task", true, repo, "", "")
	require.Error(t, err, "with routing on, an unresolvable VM name refuses the spawn")
	require.Empty(t, local.Launched(),
		"a typo'd VM must never land the worker on the local client")
}

func TestVMRoute_NilRegistryKeepsLabelOnlyBehavior(t *testing.T) {
	e, s, local := newEngine(t)
	e.VMs = nil // routing OFF — VM names are pure labels (pre-T3.3 deployments)
	e.DefaultVM = "ghost"
	e.ConfigDir = t.TempDir()
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "", "")
	require.NoError(t, err)
	require.Len(t, local.Launched(), 1, "no registry → single-client behavior unchanged")
	w, err := s.Reader().GetWorker(res.WorkerID)
	require.NoError(t, err)
	require.Equal(t, "ghost", w.VM, "the label is still recorded")
}

func TestVMRoute_SweepCorrelatesAgainstOwnVMOnly(t *testing.T) {
	e, s, local := newEngine(t)
	remote := vm.NewFake()
	e.VMs = map[string]core.VMClient{"vm1": remote}
	e.MissThreshold = 1

	id := mkRunning(t, e, s, "/wt/vmr1", "base")
	setWorkerVM(t, s, id, "vm1")
	setAgentRef(t, s, id, "pane:9")

	// Alive on ITS OWN VM — the local client seeing nothing must not matter.
	remote.Agents = []core.AgentObs{{Ref: "pane:9", Alive: true}}
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	w, err := s.Reader().GetWorker(id)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w.State, "alive on its own VM → stays running")

	// The same pane ref alive on the WRONG VM is someone else's agent: the
	// worker's own VM reports it gone, so it must finalize despite the collision.
	remote.Agents = nil
	local.Agents = []core.AgentObs{{Ref: "pane:9", Alive: true}}
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	w, err = s.Reader().GetWorker(id)
	require.NoError(t, err)
	require.NotEqual(t, core.WorkerRunning, w.State,
		"a ref collision on another VM must not keep this worker alive")
}

func TestVMRoute_KillRoutesToWorkersVM(t *testing.T) {
	e, s, local := newEngine(t)
	remote := vm.NewFake()
	e.VMs = map[string]core.VMClient{"vm1": remote}

	id := mkRunning(t, e, s, "/wt/vmr2", "base")
	setWorkerVM(t, s, id, "vm1")
	setAgentRef(t, s, id, "pane:k1")

	require.NoError(t, e.KillWorker(context.Background(), id))
	require.Contains(t, remote.Killed(), "pane:k1", "agent stop lands on the worker's VM")
	require.Empty(t, local.Killed(), "no kill on any other VM")
}
