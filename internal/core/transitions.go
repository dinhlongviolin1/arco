package core

// legalWorker maps each worker state to the set it may transition to. Terminal
// states (completed_verified, failed, killed) have no outgoing edges; `lost`
// may be re-attached to running or finalized.
var legalWorker = map[WorkerState]map[WorkerState]bool{
	WorkerStarting: set(WorkerRunning, WorkerFailed, WorkerKilled, WorkerLost),
	WorkerRunning: set(WorkerWaitingForUser, WorkerWaitingConfirmation, WorkerBlocked,
		WorkerCompletedCandidate, WorkerFailed, WorkerPaused, WorkerKilled, WorkerLost),
	WorkerWaitingForUser:      set(WorkerRunning, WorkerBlocked, WorkerPaused, WorkerFailed, WorkerKilled, WorkerLost),
	WorkerWaitingConfirmation: set(WorkerRunning, WorkerBlocked, WorkerPaused, WorkerFailed, WorkerKilled, WorkerLost),
	WorkerBlocked:             set(WorkerRunning, WorkerWaitingForUser, WorkerWaitingConfirmation, WorkerPaused, WorkerFailed, WorkerKilled, WorkerLost),
	WorkerCompletedCandidate:  set(WorkerCompletedVerified, WorkerRunning, WorkerFailed, WorkerKilled),
	WorkerPaused:              set(WorkerRunning, WorkerKilled, WorkerLost),
	WorkerLost:                set(WorkerRunning, WorkerFailed, WorkerKilled),
	WorkerCompletedVerified:   {},
	WorkerFailed:              {},
	WorkerKilled:              {},
}

// LegalWorkerTransition reports whether from→to is allowed. A self-transition
// (from==to) is always legal (idempotent re-assert).
func LegalWorkerTransition(from, to WorkerState) bool {
	if from == to {
		return true
	}
	return legalWorker[from][to]
}

var legalSession = map[SessionStatus]map[SessionStatus]bool{
	SessionOpen:     set2(SessionActive, SessionWaiting, SessionIdle, SessionDone, SessionArchived),
	SessionActive:   set2(SessionWaiting, SessionIdle, SessionDone, SessionArchived),
	SessionWaiting:  set2(SessionActive, SessionIdle, SessionDone, SessionArchived),
	SessionIdle:     set2(SessionActive, SessionWaiting, SessionDone, SessionArchived),
	SessionDone:     set2(SessionArchived), // no done→active reopen (build-guide / Task 20)
	SessionArchived: {},
}

// LegalSessionTransition reports whether from→to is allowed (self is legal).
func LegalSessionTransition(from, to SessionStatus) bool {
	if from == to {
		return true
	}
	return legalSession[from][to]
}

func set(states ...WorkerState) map[WorkerState]bool {
	m := make(map[WorkerState]bool, len(states))
	for _, s := range states {
		m[s] = true
	}
	return m
}

func set2(states ...SessionStatus) map[SessionStatus]bool {
	m := make(map[SessionStatus]bool, len(states))
	for _, s := range states {
		m[s] = true
	}
	return m
}
