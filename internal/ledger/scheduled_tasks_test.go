package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func makeTask(t *testing.T, s *Store, id, name string, next time.Time, enabled bool) {
	t.Helper()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateScheduledTask(core.ScheduledTask{
			ID: id, Name: name, Schedule: "30m", Prompt: "check the fleet",
			SessionID: "S1", Enabled: enabled, NextRun: next,
		})
	}))
}

func TestScheduled_CreateAndGet(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	now := time.Now().UTC()
	makeTask(t, s, "T1", "fleet watch", now.Add(30*time.Minute), true)

	got, err := s.Reader().GetScheduledTask("T1")
	require.NoError(t, err)
	require.Equal(t, "fleet watch", got.Name)
	require.Equal(t, "S1", got.SessionID)
	require.True(t, got.Enabled)
	require.Equal(t, "check the fleet", got.Prompt)
	require.WithinDuration(t, now.Add(30*time.Minute), got.NextRun, time.Millisecond)
	require.True(t, got.LastRun.IsZero(), "never run yet")

	_, err = s.Reader().GetScheduledTask("nope")
	require.ErrorIs(t, err, core.ErrNotFound)
}

func TestScheduled_DueOnlyEnabledAndPast(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	now := time.Now().UTC()
	makeTask(t, s, "PAST", "due", now.Add(-time.Minute), true)  // due
	makeTask(t, s, "FUT", "not yet", now.Add(time.Hour), true)  // future
	makeTask(t, s, "OFF", "paused", now.Add(-time.Hour), false) // disabled

	due, err := s.Reader().DueScheduledTasks(now, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, "PAST", due[0].ID)
}

func TestScheduled_RecordRunAdvances(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	now := time.Now().UTC()
	makeTask(t, s, "T1", "watch", now.Add(-time.Minute), true)

	next := now.Add(30 * time.Minute)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.RecordScheduledRun("T1", now, next, "ok", "2 workers healthy")
	}))
	got, _ := s.Reader().GetScheduledTask("T1")
	require.WithinDuration(t, now, got.LastRun, time.Millisecond)
	require.WithinDuration(t, next, got.NextRun, time.Millisecond)
	require.Equal(t, "ok", got.LastStatus)
	require.Equal(t, "2 workers healthy", got.LastResult)

	// after advancing, it's no longer due now
	due, _ := s.Reader().DueScheduledTasks(now, 10)
	require.Empty(t, due)
}

func TestScheduled_EnableDisableDelete(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	now := time.Now().UTC()
	makeTask(t, s, "T1", "watch", now.Add(-time.Minute), true)

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.SetScheduledTaskEnabled("T1", false) }))
	due, _ := s.Reader().DueScheduledTasks(now, 10)
	require.Empty(t, due, "disabled task is not due")

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.SetScheduledTaskEnabled("T1", true) }))
	due, _ = s.Reader().DueScheduledTasks(now, 10)
	require.Len(t, due, 1)

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error { return tx.DeleteScheduledTask("T1") }))
	_, err := s.Reader().GetScheduledTask("T1")
	require.ErrorIs(t, err, core.ErrNotFound)

	// operations on a missing task report not-found
	err = s.WithTx(context.Background(), func(tx core.Tx) error { return tx.SetScheduledTaskEnabled("gone", true) })
	require.ErrorIs(t, err, core.ErrNotFound)
}

func TestScheduled_PromptScrubbedAtWrite(t *testing.T) {
	s := newTestStore(t)
	s.SetScrubber(needleScrub{needle: "sk-SECRET"})
	seedContextSession(t, s, "S1")
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateScheduledTask(core.ScheduledTask{
			ID: "T1", Name: "x", Schedule: "30m", SessionID: "S1", Enabled: true,
			NextRun: time.Now().UTC(), Prompt: "use token sk-SECRET to check",
		})
	}))
	got, _ := s.Reader().GetScheduledTask("T1")
	require.NotContains(t, got.Prompt, "sk-SECRET", "task prompt is scrubbed at write")
	require.Contains(t, got.Prompt, "[redacted]")
}

// Deterministic sub-second due boundary (RFC3339Nano isn't lexically ordered).
func TestScheduled_DueSubSecondBoundary(t *testing.T) {
	s := newTestStore(t)
	var clk time.Time
	s.SetClock(func() time.Time { return clk })
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clk = base
	seedContextSession(t, s, "S1")

	// next_run = base + 100ms; "…05.1Z" sorts before "…05.12Z" as a string.
	makeTask(t, s, "T1", "x", base.Add(100*time.Millisecond), true)

	// at base+120ms the task is due (exact compare), despite the lexical quirk.
	due, err := s.Reader().DueScheduledTasks(base.Add(120*time.Millisecond), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)

	// at base+50ms it is NOT yet due.
	due, err = s.Reader().DueScheduledTasks(base.Add(50*time.Millisecond), 10)
	require.NoError(t, err)
	require.Empty(t, due)
}
