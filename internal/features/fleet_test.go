package features

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

type fakeLedger struct {
	workers  []core.Worker
	sessions []core.Session
	escs     []core.Escalation
}

func (f fakeLedger) ListWorkers(core.WorkerFilter) ([]core.Worker, error)   { return f.workers, nil }
func (f fakeLedger) ListSessions(core.SessionFilter) ([]core.Session, error) {
	return f.sessions, nil
}
func (f fakeLedger) ListEscalations(core.EscalationFilter) ([]core.Escalation, error) {
	return f.escs, nil
}

func run(f feature.Feature) string {
	out, _ := f.Command.Run(context.Background(), feature.CmdInput{})
	return out
}

func TestWorkers_ActiveOnly(t *testing.T) {
	l := fakeLedger{workers: []core.Worker{
		{ID: "01AAAA1111", State: core.WorkerRunning, Task: "do the thing"},
		{ID: "01BBBB2222", State: core.WorkerKilled, Task: "finished work"}, // terminal → excluded
	}}
	f := Workers(l)
	require.Equal(t, feature.BrainSafe, f.Tool.Access)
	out := run(f)
	require.Contains(t, out, "workers (1 active)")
	require.Contains(t, out, "do the thing")
	require.NotContains(t, out, "finished work", "terminal workers are excluded")
	// command and tool share the impl
	tout, _ := f.Tool.Call(context.Background(), nil)
	require.Equal(t, out, tout)
}

func TestWorkers_Empty(t *testing.T) {
	require.Equal(t, "no active workers", run(Workers(fakeLedger{})))
}

// A long task is elided with the features-package inline "…" (documented to
// differ from the old telegram "\n… (truncated)" suffix).
func TestWorkers_LongTaskElided(t *testing.T) {
	long := "refactor the entire authentication subsystem end to end with tests"
	l := fakeLedger{workers: []core.Worker{{ID: "01AAAA1111", State: core.WorkerRunning, Task: long}}}
	out := run(Workers(l))
	require.Contains(t, out, "…")
	require.NotContains(t, out, "(truncated)", "features uses the inline ellipsis, not the old telegram suffix")
	require.NotContains(t, out, long, "the full over-long task is not shown verbatim")
}

func TestSessions_ActiveOnly(t *testing.T) {
	topic := int64(7)
	l := fakeLedger{sessions: []core.Session{
		{ID: "S1", Slug: "fix-auth", Status: core.SessionActive, TGTopicID: &topic},
		{ID: "S2", Slug: "old", Status: core.SessionDone},                 // excluded
		{ID: "P1", Kind: core.SessionKindPool, Status: core.SessionActive}, // pool excluded
	}}
	out := run(Sessions(l))
	require.Contains(t, out, "sessions (1)")
	require.Contains(t, out, "fix-auth")
	require.Contains(t, out, "topic set")
	require.NotContains(t, out, "old")
}

func TestSessions_Empty(t *testing.T) {
	require.Contains(t, run(Sessions(fakeLedger{})), "no active sessions")
}

func TestVMs_ListsFleet(t *testing.T) {
	f := VMs([]string{"local (default · this box)", "vm1 (host vm1)"})
	require.Equal(t, feature.BrainSafe, f.Tool.Access)
	out := run(f)
	require.Contains(t, out, "attached VMs (2)")
	require.Contains(t, out, "vm1 (host vm1)")
	// command and tool share the impl
	tout, _ := f.Tool.Call(context.Background(), nil)
	require.Equal(t, out, tout)
}

func TestVMs_None(t *testing.T) {
	require.Equal(t, "no VMs configured", run(VMs(nil)))
}

func TestStatus_EstopAndCounts(t *testing.T) {
	l := fakeLedger{
		workers: []core.Worker{{ID: "W1", State: core.WorkerRunning}, {ID: "W2", State: core.WorkerRunning}},
		escs:    []core.Escalation{{ID: "E1"}},
	}
	out := run(Status(l, func() bool { return true }))
	require.Contains(t, out, "ESTOP ENGAGED")
	require.Contains(t, out, "active workers: 2")
	require.Contains(t, out, "running×2")
	require.Contains(t, out, "pending decisions: 1")
}

func TestStatus_Running(t *testing.T) {
	out := run(Status(fakeLedger{}, func() bool { return false }))
	require.Contains(t, out, "▶️ running")
	require.Contains(t, out, "active workers: 0")
}

// Status tolerates a nil paused func (defaults to running).
func TestStatus_NilPaused(t *testing.T) {
	out := run(Status(fakeLedger{}, nil))
	require.Contains(t, out, "running")
}
