package fusion

import (
	"testing"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name string
		cur  core.WorkerState
		sig  Signals
		want core.WorkerState
		amb  bool
	}{
		{"blocked+alive", core.WorkerRunning, Signals{HerdrState: "blocked", Alive: true}, core.WorkerBlocked, false},
		{"idle+headchanged->candidate", core.WorkerRunning, Signals{HerdrState: "idle", Alive: true, HeadChanged: true}, core.WorkerCompletedCandidate, false},
		// idle but ALIVE with no progress is the normal between-turns state — it must
		// NOT finalize a live worker `failed` (push-vs-sweep consistency); keep the
		// current state and flag ambiguous so the brain classifies.
		{"idle+alive+nohead->ambiguous keeps running", core.WorkerRunning, Signals{HerdrState: "idle", Alive: true}, core.WorkerRunning, true},
		{"done+nohead->failed", core.WorkerRunning, Signals{HerdrState: "done", Alive: true}, core.WorkerFailed, false},
		{"dead+nohead->failed", core.WorkerRunning, Signals{Alive: false}, core.WorkerFailed, false},
		{"waiting input->waiting_for_user", core.WorkerRunning, Signals{HerdrState: "idle", Alive: true, WaitingInput: true}, core.WorkerWaitingForUser, false},
		{"danger wait->waiting_for_confirmation", core.WorkerRunning, Signals{Alive: true, WaitingInput: true, DangerWait: true}, core.WorkerWaitingConfirmation, false},
		{"working+alive->running", core.WorkerStarting, Signals{HerdrState: "working", Alive: true}, core.WorkerRunning, false},
		{"unknown->ambiguous keeps cur", core.WorkerRunning, Signals{HerdrState: "", Alive: true}, core.WorkerRunning, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, amb := Resolve(c.cur, c.sig)
			if got != c.want || amb != c.amb {
				t.Errorf("Resolve(%s,%+v)=(%s,%v) want (%s,%v)", c.cur, c.sig, got, amb, c.want, c.amb)
			}
		})
	}
}
