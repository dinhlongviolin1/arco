// Supplementary T2.4 tests: the budget must hold for inputs whose ENCODED size
// isn't bounded by their cap, and the tail must shed its OLDEST events first.
package reconcile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// A task made entirely of characters %q escapes (each byte → \" or \x00) encodes
// several times larger than fieldCap. The backstop trim must still land the
// prompt inside the budget without costing the newest event or the trailer.
func TestAssembleContext_BudgetHoldsUnderQuoteEscaping(t *testing.T) {
	evil := strings.Repeat("\"\x00\\", contextBudget)
	w := core.Worker{ID: "W1", State: core.WorkerRunning, Task: evil}
	s := core.Session{Goal: evil, Facts: evil, ContextSummary: evil}
	events := []core.Event{
		{ID: 1, Kind: "old_event", Payload: evil},
		{ID: 2, Kind: "newest_event", Payload: `{"k":"v"}`},
	}
	a := assembleContext(w, s, events, evil, evil)

	require.LessOrEqual(t, len(a), contextBudget)
	require.Contains(t, a, "newest_event")
	require.True(t, strings.HasSuffix(a, stepInstruction), "trailer is never trimmed")
	require.Equal(t, a, assembleContext(w, s, events, evil, evil), "byte-stable under trimming")
}

// When the tail can't fit whole, the OLDEST events are dropped and the survivors
// stay in oldest→newest order.
func TestAssembleContext_DropsOldestEventsFirst(t *testing.T) {
	var events []core.Event
	for i := 1; i <= 200; i++ {
		events = append(events, core.Event{ID: int64(i), Kind: "bulk", Payload: strings.Repeat("p", eventPayloadCap)})
	}
	a := assembleContext(core.Worker{ID: "W", State: core.WorkerRunning}, core.Session{}, events, "", "")

	require.LessOrEqual(t, len(a), contextBudget)
	require.NotContains(t, a, "[1] bulk", "the oldest events are the ones shed")
	require.Contains(t, a, "[200] bulk", "the newest event always survives")
	require.Contains(t, a, "[199] bulk")
	require.Less(t, strings.Index(a, "[199] bulk"), strings.Index(a, "[200] bulk"), "emitted oldest→newest")
}

// A blank (or whitespace-only) memory file contributes no dangling header.
func TestAssembleContext_BlankSectionsLeaveNoHeader(t *testing.T) {
	a := assembleContext(core.Worker{ID: "W", State: core.WorkerRunning}, core.Session{Facts: "  \n\t "},
		nil, "\n\n", "   ")
	require.NotContains(t, a, "Session facts:")
	require.NotContains(t, a, "User memory:")
	require.NotContains(t, a, "Memory index:")
}
