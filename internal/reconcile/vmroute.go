package reconcile

import (
	"fmt"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// vmFor resolves the VMClient that owns the named VM (rev7/T3.3). With no
// registry (e.VMs == nil) routing is off and every name is a pure label on the
// single client; with a registry, "" is the daemon-local client and a named VM
// MUST have an entry — a missing one is ErrUnknownVM, never a silent fallback
// to the local client (a typo'd VM must not land ops on the wrong machine).
func (e *Engine) vmFor(name string) (core.VMClient, error) {
	if e.VMs == nil || name == "" {
		return e.VM, nil
	}
	c, ok := e.VMs[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q has no [[vms]] entry", core.ErrUnknownVM, name)
	}
	return c, nil
}

// sweepGroup is one VM's slice of a sweep: the client to observe it with (nil =
// unresolvable VM → unobservable, never finalized on), every worker assigned to
// it (the orphan reaper's input), the liveness-tracked subset, and the subset's
// worktrees for that VM's single GitHeads call.
type sweepGroup struct {
	client    core.VMClient
	all       []core.Worker
	live      []core.Worker
	worktrees map[string]bool
}

// sweepGroups partitions the fleet by owning VM for the sweep. With no registry
// every worker lands in ONE group on e.VM (VM names are pure labels — pre-T3.3
// behavior); with a registry the group key is the worker's VM name, resolved
// through vmFor ("" → e.VM).
func (e *Engine) sweepGroups(all []core.Worker, pendingEsc map[string]bool) map[string]*sweepGroup {
	groups := map[string]*sweepGroup{}
	for _, w := range all {
		key := ""
		if e.VMs != nil {
			key = w.VM
		}
		g := groups[key]
		if g == nil {
			g = &sweepGroup{worktrees: map[string]bool{}}
			g.client, _ = e.vmFor(key) // nil on a registry gap → unobservable
			groups[key] = g
		}
		g.all = append(g.all, w)
		// Workers whose agent should no longer be running (agentReclaimable) are
		// reaper-only: their agent is intentionally reclaimed, so its absence is
		// EXPECTED, not a liveness death — liveness-tracking them would finalize a
		// paused worker to `lost` and defeat the pause.
		if agentReclaimable(w, pendingEsc) {
			continue
		}
		g.live = append(g.live, w)
		g.worktrees[headKey(w)] = true
	}
	return groups
}
