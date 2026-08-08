package reconcile

import "github.com/dinhlongviolin1/arco/internal/core"

// sessionMode is the D9 gate lookup: the supervision mode of a session, read
// via the Reader. On ANY error (missing session, closed store) it returns
// ModeAssist — fail toward the default, never toward more autonomy. The pool
// sentinel session likewise just reads as its stored/default mode.
func (e *Engine) sessionMode(sessionID string) core.SupervisionMode {
	s, err := e.Store.Reader().GetSession(sessionID)
	if err != nil {
		return core.ModeAssist
	}
	m, err := core.ParseSupervisionMode(string(s.SupervisionMode))
	if err != nil {
		return core.ModeAssist // unrecognized stored value → the safe default
	}
	return m
}

// workerMode resolves a worker's owning session's supervision mode. Same
// fail-toward-assist posture as sessionMode when the worker can't be read.
func (e *Engine) workerMode(workerID string) core.SupervisionMode {
	w, err := e.Store.Reader().GetWorker(workerID)
	if err != nil {
		return core.ModeAssist
	}
	return e.sessionMode(w.OwnerSession)
}
