package reconcile

import "os"

// Paused reports whether the operator's emergency stop is engaged: the ESTOP
// sentinel file exists next to the ledger (written by `arco pause`, removed by
// `arco resume`). File-based on purpose — it works even when the daemon or its
// socket is wedged, and one os.Stat per check needs no caching or invalidation.
//
// Semantics are PAUSE-NEW-WORK, NEVER-KILL-IN-FLIGHT: admission (dispatch/
// spawn/delegate), brain classification, earn-out auto-answers, CI polling,
// merge-queue processing, activity-back-off restores, and the orphan reaper
// all stand down; liveness observation, state finalization, and every
// operator-initiated action (answer/confirm/kill/redeliver) keep working.
// Any stat outcome other than "definitely absent" counts as ENGAGED — a
// corrupt or unreadable sentinel must fail toward stopped, not toward running.
func (e *Engine) Paused() bool {
	if e.EStopPath == "" {
		return false
	}
	_, err := os.Stat(e.EStopPath)
	return err == nil || !os.IsNotExist(err)
}
