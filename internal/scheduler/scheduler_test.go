package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
)

func newStore(t *testing.T) *ledger.Store {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.Migrate(context.Background()))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: "S1", Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))
	return s
}

func addTask(t *testing.T, s *ledger.Store, id, schedule string, next time.Time) {
	t.Helper()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateScheduledTask(core.ScheduledTask{
			ID: id, Name: id, Schedule: schedule, Prompt: "check fleet",
			SessionID: "S1", Enabled: true, NextRun: next,
		})
	}))
}

// A due task fires once, advances to its next run, and records status/result.
func TestFireDue_RunsAndAdvances(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	addTask(t, s, "T1", "30m", now.Add(-time.Minute)) // overdue

	var ran []string
	sc := &Scheduler{Store: s, Now: func() time.Time { return now },
		Run: func(_ context.Context, task core.ScheduledTask) (string, error) {
			ran = append(ran, task.ID)
			return "2 workers healthy", nil
		}}
	sc.FireDue(context.Background())

	require.Equal(t, []string{"T1"}, ran, "the due task fired once")
	got, _ := s.Reader().GetScheduledTask("T1")
	require.Equal(t, "ok", got.LastStatus)
	require.Equal(t, "2 workers healthy", got.LastResult)
	require.Equal(t, now.Add(30*time.Minute), got.NextRun, "advanced to now + interval")

	// Not due anymore on an immediate second pass.
	ran = nil
	sc.FireDue(context.Background())
	require.Empty(t, ran, "no double-fire")
}

// A run error is recorded, doesn't stop other tasks, and the task stays enabled.
func TestFireDue_ErrorRecordedNotFatal(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	addTask(t, s, "BAD", "1h", now.Add(-time.Minute))
	addTask(t, s, "OK", "1h", now.Add(-time.Minute))

	var ran []string
	sc := &Scheduler{Store: s, Now: func() time.Time { return now },
		Run: func(_ context.Context, task core.ScheduledTask) (string, error) {
			ran = append(ran, task.ID)
			if task.ID == "BAD" {
				return "", errors.New("brain unavailable")
			}
			return "ok result", nil
		}}
	sc.FireDue(context.Background())

	require.ElementsMatch(t, []string{"BAD", "OK"}, ran, "one failure doesn't skip the other")
	bad, _ := s.Reader().GetScheduledTask("BAD")
	require.Equal(t, "error", bad.LastStatus)
	require.Contains(t, bad.LastResult, "brain unavailable")
	require.True(t, bad.Enabled, "a failing task stays enabled")
	require.Equal(t, now.Add(time.Hour), bad.NextRun, "still advances")
}

// A run that outlasts its interval schedules its NEXT fire from completion time,
// not the stale start-of-pass clock — so next_run is always in the future and the
// task can't re-fire every tick (the re-fire-storm regression).
func TestFireDue_SlowRunNextIsFuture(t *testing.T) {
	s := newStore(t)
	clock := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	addTask(t, s, "SLOW", "30m", clock.Add(-time.Minute)) // overdue by 1m

	sc := &Scheduler{Store: s, Now: func() time.Time { return clock },
		Run: func(context.Context, core.ScheduledTask) (string, error) {
			clock = clock.Add(35 * time.Minute) // the run takes 35m — longer than the 30m interval
			return "done", nil
		}}
	sc.FireDue(context.Background())

	got, _ := s.Reader().GetScheduledTask("SLOW")
	require.True(t, got.NextRun.After(clock),
		"next_run (%s) must be after post-run now (%s), not in the past", got.NextRun, clock)
	require.Equal(t, clock.Add(30*time.Minute), got.NextRun, "advanced 30m from completion")

	// And it is NOT due on the immediately-following tick.
	due, _ := s.Reader().DueScheduledTasks(clock, 100)
	require.Empty(t, due, "no immediate re-fire after a slow run")
}

// A malformed schedule backs off an hour rather than wedging.
func TestFireDue_BadScheduleBacksOff(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	addTask(t, s, "T1", "not-a-schedule", now.Add(-time.Minute))
	sc := &Scheduler{Store: s, Now: func() time.Time { return now },
		Run: func(context.Context, core.ScheduledTask) (string, error) { return "x", nil }}
	sc.FireDue(context.Background())
	got, _ := s.Reader().GetScheduledTask("T1")
	require.Equal(t, now.Add(time.Hour), got.NextRun)
}
