package reconcile

import (
	"context"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// activityActor attributes every back-off mode write in the ledger, so a
// demote/restore is distinguishable from an operator's own `arco mode` flip
// (D9: the ledger is the only record of who decided what).
const activityActor = "activity-backoff"

// defaultSelfOpWindow is how long after arco touched a pane the focus/scroll
// echo herdr pushes still reads as arco-caused. A few seconds covers the
// prompt-delivery round trip (PromptReady retries past the agent's TUI boot)
// without swallowing a human who arrives right after.
const defaultSelfOpWindow = 5 * time.Second

// defaultActivityRestoreAfter is the quiet period before the back-off returns a
// session it demoted to auto. LONG on purpose: a human who touched a pane is
// plausibly still working in that repo minutes later, and restoring autonomy
// under their hands is exactly the surprise D9 exists to prevent. Erring long
// only costs autonomy, never safety.
const defaultActivityRestoreAfter = 20 * time.Minute

func (e *Engine) selfOpWindow() time.Duration {
	if e.SelfOpWindow > 0 {
		return e.SelfOpWindow
	}
	return defaultSelfOpWindow
}

func (e *Engine) activityRestoreAfter() time.Duration {
	if e.ActivityRestoreAfter > 0 {
		return e.ActivityRestoreAfter
	}
	return defaultActivityRestoreAfter
}

// NoteSelfPaneOp marks a pane as just-touched BY ARCO (prompt delivery,
// re-delivery, escalation-answer delivery). herdr pushes a focus/scroll event
// for those touches too, and mistaking arco's own echo for human presence would
// have every prompt demote its own session out of auto. Called by every
// pane-touching path in this package; safe to call with an empty/unknown pane.
func (e *Engine) NoteSelfPaneOp(paneID string) {
	if paneID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.selfOps == nil {
		e.selfOps = map[string]time.Time{}
	}
	e.selfOps[paneID] = e.Store.Now()
}

// selfCaused reports whether activity on paneID falls inside SelfOpWindow of
// arco's own last op on it; the entry is dropped once expired (the map is
// keyed by live pane, so it stays bounded by the fleet either way).
func (e *Engine) selfCaused(paneID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	at, ok := e.selfOps[paneID]
	if !ok {
		return false
	}
	if e.Store.Now().Sub(at) < e.selfOpWindow() {
		return true
	}
	delete(e.selfOps, paneID)
	return false
}

// ApplyHumanActivity feeds one herdr activity signal (pane focused / scrolled,
// pushed over the T2.1 subscription) into the D9 back-off: a human at the
// keyboard of a worker's pane means arco must get out of the way, so that
// worker's session drops auto → assist. Mirrors ApplyHerdrStatus's resolution
// (Reader ListWorkers scan by AgentRef): an unknown pane is a silent nil, since
// the operator's OWN panes flow through the same subscription and must never
// error-spam. assist/manual are operator statements — activity never touches
// them, in either direction.
func (e *Engine) ApplyHumanActivity(ctx context.Context, paneID string) error {
	if paneID == "" {
		return nil
	}
	if e.selfCaused(paneID) {
		return nil // arco's own echo, not a human
	}
	ws, err := e.Store.Reader().ListWorkers(core.WorkerFilter{})
	if err != nil {
		return err
	}
	for _, w := range ws {
		if w.AgentRef == "" || w.AgentRef != paneID {
			continue
		}
		// The activity subscription is LOCAL-ONLY (T3.3 scope, like ApplyHerdrStatus):
		// with routing on, a local pane id colliding with a remote worker's per-host
		// ref is someone else's pane — never back that session off.
		if e.VMs != nil && w.VM != "" {
			continue
		}
		return e.backOffSession(ctx, w.OwnerSession)
	}
	return nil
}

// backOffSession demotes an auto session to assist and (re)stamps its quiet
// period. The stamp is refreshed for a session the back-off ALREADY demoted
// too, so continued human presence keeps extending the quiet period; it is
// never created for a session we didn't demote, because the demotion map is
// exactly the set Sweep may restore.
func (e *Engine) backOffSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	var demoted bool
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		s, err := tx.GetSession(sessionID)
		if err != nil {
			return err
		}
		if m, perr := core.ParseSupervisionMode(string(s.SupervisionMode)); perr != nil || m != core.ModeAuto {
			return nil
		}
		if err := tx.SetSessionMode(sessionID, core.ModeAssist, activityActor); err != nil {
			return err
		}
		demoted = true
		return nil
	})
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.activityDemoted == nil {
		e.activityDemoted = map[string]time.Time{}
	}
	if _, tracked := e.activityDemoted[sessionID]; demoted || tracked {
		e.activityDemoted[sessionID] = e.Store.Now()
	}
	return nil
}

// restoreActivityBackoff returns to auto every session the back-off itself
// demoted whose pane has been quiet for ActivityRestoreAfter. Only OUR
// demotions are eligible: an operator's explicit assist is a standing decision
// and is NEVER auto-promoted.
//
// The demotion set is in-memory (like misses/redriving). A daemon restart
// therefore forgets pending restores and leaves those sessions in assist —
// which fails toward LESS autonomy, the correct direction for a D9 control;
// the operator restores with `arco mode <session> auto`.
func (e *Engine) restoreActivityBackoff(ctx context.Context) int {
	type due struct {
		sid string
		at  time.Time
	}
	cutoff := e.Store.Now().Add(-e.activityRestoreAfter())
	var ready []due
	e.mu.Lock()
	for sid, at := range e.activityDemoted {
		if !at.After(cutoff) {
			ready = append(ready, due{sid, at})
		}
	}
	e.mu.Unlock()

	n := 0
	for _, d := range ready {
		err := e.Store.WithTx(ctx, func(tx core.Tx) error {
			s, err := tx.GetSession(d.sid)
			if err != nil {
				return err
			}
			// The operator may have moved the session since we demoted it (to manual,
			// or back to auto by hand) — only undo what still looks like our demotion.
			if m, perr := core.ParseSupervisionMode(string(s.SupervisionMode)); perr != nil || m != core.ModeAssist {
				return nil
			}
			return tx.SetSessionMode(d.sid, core.ModeAuto, activityActor)
		})
		if err != nil {
			continue // keep the entry; the next sweep retries
		}
		e.mu.Lock()
		// Forget the demotion unless fresh activity re-stamped it while the tx ran
		// (then the entry stands and the next activity event re-demotes).
		if at, ok := e.activityDemoted[d.sid]; ok && at.Equal(d.at) {
			delete(e.activityDemoted, d.sid)
		}
		e.mu.Unlock()
		n++
	}
	return n
}
