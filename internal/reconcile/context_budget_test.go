// Guideline tests for T2.4: the brain decision prompt gains the session's
// durable context — Facts + ContextSummary (already in the ledger, previously
// NEVER assembled) — and the always-hot memory tier-1/2 (USER.md + MEMORY.md),
// under a total byte BUDGET that trims rather than starves: huge fields are
// capped, the event tail survives, and the whole prompt stays byte-stable.
//
// Pinned API: assembleContext(w, s, events, userMD, indexMD) — the memory text
// is passed in (loaded best-effort by the Engine via its Memory store seam),
// keeping the assembler pure/byte-stable. Existing context_test.go call sites
// may be MECHANICALLY updated (append `, "", ""`), assertions untouched.
package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/memory"
)

// Facts, ContextSummary, and both memory tiers appear under their pinned
// section markers when set.
func TestAssembleContext_FactsSummaryAndMemoryPresent(t *testing.T) {
	w := core.Worker{ID: "W1", State: core.WorkerRunning, Task: "fix the bug"}
	s := core.Session{
		ID: "S1", Goal: "ship it",
		Facts:          "repo uses Go 1.24; CI is GitHub Actions",
		ContextSummary: "worker previously fixed the parser, tests were green",
	}
	a := assembleContext(w, s, nil, "operator prefers squash merges", "- [arco](arco.md) — daemon notes")

	require.Contains(t, a, "Session facts:")
	require.Contains(t, a, "repo uses Go 1.24; CI is GitHub Actions")
	require.Contains(t, a, "Context summary:")
	require.Contains(t, a, "worker previously fixed the parser")
	require.Contains(t, a, "User memory:")
	require.Contains(t, a, "operator prefers squash merges")
	require.Contains(t, a, "Memory index:")
	require.Contains(t, a, "daemon notes")
	require.Contains(t, a, "JSON StepResult", "instruction trailer survives")

	b := assembleContext(w, s, nil, "operator prefers squash merges", "- [arco](arco.md) — daemon notes")
	require.Equal(t, a, b, "byte-stable with all new sections populated")
}

// Empty inputs degrade gracefully: no stray section headers, exactly like the
// pre-T2.4 minimal prompt.
func TestAssembleContext_EmptySectionsOmitted(t *testing.T) {
	a := assembleContext(core.Worker{ID: "W", State: core.WorkerStarting}, core.Session{}, nil, "", "")
	require.NotContains(t, a, "Session facts:")
	require.NotContains(t, a, "Context summary:")
	require.NotContains(t, a, "User memory:")
	require.NotContains(t, a, "Memory index:")
	require.Contains(t, a, "JSON StepResult")
}

// The total prompt is BUDGETED: adversarially huge facts/summary/memory/events
// cannot blow the prompt past contextBudget, and the budget is generous enough
// to be real (≥ 8 KiB).
func TestAssembleContext_BudgetRespected(t *testing.T) {
	require.GreaterOrEqual(t, contextBudget, 8192, "budget must fit a real decision context")

	huge := strings.Repeat("m", contextBudget) // each field alone would bust the budget
	w := core.Worker{ID: "W1", State: core.WorkerRunning, Task: huge}
	s := core.Session{Goal: huge, Facts: huge, ContextSummary: huge}
	events := []core.Event{
		{ID: 1, Kind: "old_event", Payload: huge},
		{ID: 2, Kind: "newest_event", Payload: `{"k":"v"}`},
	}
	a := assembleContext(w, s, events, huge, huge)

	require.LessOrEqual(t, len(a), contextBudget, "total prompt must not exceed the budget")
	// Trim, don't starve: the newest event is the highest-signal input and must
	// survive even when every prose field is oversized.
	require.Contains(t, a, "newest_event")
	require.Contains(t, a, "JSON StepResult", "instruction trailer must never be trimmed away")
}

// The event tail is budget-based, not hard-capped at the old 20: many small
// events all fit when the budget allows.
func TestAssembleContext_EventTailNotHardCapped(t *testing.T) {
	var events []core.Event
	for i := 1; i <= 60; i++ {
		events = append(events, core.Event{ID: int64(i), Kind: "tick", Payload: "{}"})
	}
	a := assembleContext(core.Worker{ID: "W", State: core.WorkerRunning}, core.Session{}, events, "", "")
	require.Contains(t, a, "[1]", "oldest of 60 small events still present")
	require.Contains(t, a, "[60]", "newest present")
	require.Contains(t, a, "[37]", "no silent 20-event cap in assembly")
}

// End-to-end: a session's stored Facts/ContextSummary and the Engine's memory
// store (tier 1 USER.md + tier 2 MEMORY.md) reach the brain runner's prompt.
func TestBrain_FactsSummaryMemoryReachRunner(t *testing.T) {
	var gotPrompt string
	e, s, _ := newEngine(t)
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			gotPrompt = strings.Join(args, " ")
			return []byte(`{"kind":"final_output","reason":"done"}`), nil
		}}

	memDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(memDir, "USER.md"), []byte("operator: Long, solo maintainer"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("- [herdr](herdr.md) — pane contract"), 0o600))
	e.Memory = memory.New(memDir)

	res, err := e.Dispatch(context.Background(), "", "implement the feature", true)
	require.NoError(t, err)
	_, err = s.DB().Exec(`UPDATE sessions SET facts=?, context_summary=? WHERE id=?`,
		"target box runs Debian", "prior attempt failed on flaky test", res.SessionID)
	require.NoError(t, err)

	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(res.WorkerID)))
	e.Exec.Wait()

	require.Contains(t, gotPrompt, "target box runs Debian", "session Facts reach the brain")
	require.Contains(t, gotPrompt, "prior attempt failed on flaky test", "ContextSummary reaches the brain")
	require.Contains(t, gotPrompt, "operator: Long, solo maintainer", "memory tier 1 reaches the brain")
	require.Contains(t, gotPrompt, "pane contract", "memory tier 2 reaches the brain")
}

// A missing/empty memory dir must not break classification (best-effort load).
func TestBrain_NoMemoryStoreStillClassifies(t *testing.T) {
	var calls int
	e, _, _ := newEngine(t)
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			calls++
			return []byte(`{"kind":"final_output","reason":"done"}`), nil
		}}
	e.Memory = memory.New(filepath.Join(t.TempDir(), "does-not-exist"))

	res, err := e.Dispatch(context.Background(), "", "some task", true)
	require.NoError(t, err)
	require.NoError(t, e.ApplyEvent(context.Background(), ambiguousEvent(res.WorkerID)))
	e.Exec.Wait()
	require.Equal(t, 1, calls, "classification proceeds without memory")
}
