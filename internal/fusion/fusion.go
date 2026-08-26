// Package fusion resolves a worker's authoritative state from multiple signals
// (herdr hook state, process liveness, git HEAD change, transcript wait
// markers). It is a pure function so it is trivially testable and never blocks.
package fusion

import "github.com/dinhlongviolin1/arco/internal/core"

// Signals is the observed evidence about one worker at a point in time.
type Signals struct {
	HerdrState   string // working|running|idle|done|blocked|"" (unknown)
	Alive        bool
	HeadChanged  bool
	WaitingInput bool // transcript shows a prompt awaiting human input
	DangerWait   bool // the pending input is a danger-class confirm
}

// Resolve computes the target state and whether the signals were ambiguous
// (disagree / unknown). Precedence: explicit input/wait markers > herdr state >
// liveness > HEAD. When nothing is conclusive it keeps the current state and
// flags ambiguity (so the caller can ask the brain to classify).
func Resolve(cur core.WorkerState, s Signals) (target core.WorkerState, ambiguous bool) {
	switch {
	case s.WaitingInput && s.DangerWait:
		return core.WorkerWaitingConfirmation, false
	case s.WaitingInput:
		return core.WorkerWaitingForUser, false
	case s.HerdrState == "blocked":
		return core.WorkerBlocked, false
	case s.HerdrState == "working" || s.HerdrState == "running":
		if s.Alive {
			return core.WorkerRunning, false
		}
		return finished(s)
	case s.HerdrState == "done" || !s.Alive:
		// Truly terminal: the agent reported done, or liveness says it's gone.
		return finished(s)
	case s.HerdrState == "idle":
		// idle but ALIVE is the normal between-turns / awaiting-input state, NOT a
		// terminal one — the sweep's liveness treats idle as alive too (only `done`
		// is terminal). Lumping idle with done/dead here finalized a live worker
		// `failed` on the push stream the moment it paused between turns (review:
		// push-vs-sweep inconsistency). If it committed, that's a candidate; else
		// it's ambiguous — keep the current state and let the brain classify (the
		// designed handling), never an unconditional failed.
		if s.HeadChanged {
			return core.WorkerCompletedCandidate, false
		}
		return cur, true
	default:
		return cur, true // signals inconclusive
	}
}

// finished maps a terminated worker to candidate (made progress) or failed.
func finished(s Signals) (core.WorkerState, bool) {
	if s.HeadChanged {
		return core.WorkerCompletedCandidate, false
	}
	return core.WorkerFailed, false
}
