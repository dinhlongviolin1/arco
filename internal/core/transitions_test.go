package core

import "testing"

func TestLegalWorkerTransition(t *testing.T) {
	cases := []struct {
		from, to WorkerState
		want     bool
	}{
		{WorkerStarting, WorkerRunning, true},
		{WorkerRunning, WorkerCompletedCandidate, true},
		{WorkerCompletedCandidate, WorkerCompletedVerified, true},
		{WorkerRunning, WorkerPaused, true},
		{WorkerPaused, WorkerRunning, true},
		{WorkerLost, WorkerRunning, true},    // re-attached by sweep
		{WorkerRunning, WorkerRunning, true}, // idempotent self
		// illegal:
		{WorkerKilled, WorkerRunning, false},
		{WorkerCompletedVerified, WorkerRunning, false},
		{WorkerFailed, WorkerRunning, false},
		{WorkerStarting, WorkerCompletedVerified, false},
	}
	for _, c := range cases {
		if got := LegalWorkerTransition(c.from, c.to); got != c.want {
			t.Errorf("LegalWorkerTransition(%s,%s)=%v want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestLegalSessionTransition(t *testing.T) {
	if !LegalSessionTransition(SessionOpen, SessionActive) {
		t.Error("open->active should be legal")
	}
	if !LegalSessionTransition(SessionDone, SessionArchived) {
		t.Error("done->archived should be legal")
	}
	if LegalSessionTransition(SessionDone, SessionActive) {
		t.Error("done->active must be illegal (no reopen)")
	}
	if LegalSessionTransition(SessionArchived, SessionActive) {
		t.Error("archived is terminal")
	}
}

func TestWorkerStateTerminal(t *testing.T) {
	for _, s := range []WorkerState{WorkerCompletedVerified, WorkerFailed, WorkerKilled, WorkerLost} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []WorkerState{WorkerStarting, WorkerRunning, WorkerPaused, WorkerBlocked} {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}
