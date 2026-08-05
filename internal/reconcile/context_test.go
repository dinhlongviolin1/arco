package reconcile

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestAssembleContext_ByteStableAndComplete(t *testing.T) {
	w := core.Worker{ID: "W1", State: core.WorkerRunning, Task: "fix the bug", OwnerSession: "S1"}
	s := core.Session{ID: "S1", Goal: "ship the release"}
	events := []core.Event{
		{ID: 3, Kind: "dispatch_done", Payload: "{}"},
		{ID: 7, Kind: "state_change", Payload: `{"target":"running"}`},
	}
	a := assembleContext(w, s, events)
	b := assembleContext(w, s, events)
	require.Equal(t, a, b, "same inputs → identical bytes (byte-stable for prompt_hash)")

	require.Contains(t, a, "Worker W1 state=running")
	require.Contains(t, a, `task="fix the bug"`)
	require.Contains(t, a, "Session goal: ship the release")
	require.Contains(t, a, "[3] dispatch_done")
	require.Contains(t, a, "[7] state_change")
	// event order is oldest→newest (id 3 before id 7)
	require.Less(t, strings.Index(a, "[3]"), strings.Index(a, "[7]"))
	require.Contains(t, a, "JSON StepResult")
}

func TestAssembleContext_ChildShowsLineageAndTruncates(t *testing.T) {
	w := core.Worker{ID: "C1", State: core.WorkerRunning, Task: "sub", DelegationDepth: 2, ParentWorkerID: "P1"}
	big := strings.Repeat("x", eventPayloadCap+50)
	a := assembleContext(w, core.Session{}, []core.Event{{ID: 1, Kind: "note", Payload: big}})
	require.Contains(t, a, "depth=2 parent=P1")
	require.Contains(t, a, "…(truncated)")
	require.NotContains(t, a, big, "oversized payload is truncated, not embedded whole")
}

func TestAssembleContext_MinimalWhenEmpty(t *testing.T) {
	// no goal, no events → still a valid, deterministic prompt (no panic, no stray lines)
	a := assembleContext(core.Worker{ID: "W", State: core.WorkerStarting}, core.Session{}, nil)
	require.Contains(t, a, "Worker W state=starting")
	require.NotContains(t, a, "Session goal:")
	require.NotContains(t, a, "Recent events")
	require.Contains(t, a, "JSON StepResult")
}

// A huge Task/Goal is capped (bounds prompt size), and truncation never splits a
// multi-byte rune (output stays valid UTF-8).
func TestAssembleContext_CapsFieldsAndRuneSafe(t *testing.T) {
	huge := strings.Repeat("é", fieldCap) // 2-byte runes; a byte-slice at fieldCap would split one
	w := core.Worker{ID: "W", State: core.WorkerRunning, Task: huge}
	a := assembleContext(w, core.Session{Goal: huge}, nil)
	require.Contains(t, a, "…(truncated)")
	require.Less(t, len(a), 2*len(huge), "oversized task+goal are capped, not embedded whole twice")
	require.True(t, utf8.ValidString(a), "truncation must not split a rune → prompt stays valid UTF-8")
}

// The richer context actually reaches the brain runner (session goal + event tail).
func TestBrain_ContextReachesRunner(t *testing.T) {
	var gotPrompt string
	e, s, _ := newEngine(t)
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			gotPrompt = strings.Join(args, " ") // the assembled prompt rides in the args (the -p value)
			return []byte(`{"kind":"final_output","reason":"done"}`), nil
		}}
	// Dispatch creates the session with Goal=task, so the goal appears in context.
	res, err := e.Dispatch(context.Background(), "", "implement the feature", true)
	require.NoError(t, err)
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(res.WorkerID)))
	e.Exec.Wait()

	require.Contains(t, gotPrompt, "Session goal: implement the feature")
	require.Contains(t, gotPrompt, "Recent events", "the event tail is included")
	_ = s
}
