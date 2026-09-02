package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// fakeTasks is an in-memory telegram.TaskStore for command tests.
type fakeTasks struct {
	tasks     []core.ScheduledTask
	seq       int
	createErr error
}

func (f *fakeTasks) CreateTask(_ context.Context, name, schedule, prompt string, next time.Time) (core.ScheduledTask, error) {
	if f.createErr != nil {
		return core.ScheduledTask{}, f.createErr
	}
	f.seq++
	t := core.ScheduledTask{
		ID: "TASKID000" + string(rune('A'+f.seq)), Name: name, Schedule: schedule,
		Prompt: prompt, SessionID: "SESS" + string(rune('A'+f.seq)), Enabled: true, NextRun: next,
	}
	f.tasks = append(f.tasks, t)
	return t, nil
}

func (f *fakeTasks) ListTasks() ([]core.ScheduledTask, error) { return f.tasks, nil }

func (f *fakeTasks) SetTaskEnabled(_ context.Context, id string, on bool) error {
	for i := range f.tasks {
		if f.tasks[i].ID == id {
			f.tasks[i].Enabled = on
			return nil
		}
	}
	return nil
}

func (f *fakeTasks) DeleteTask(_ context.Context, id string) error {
	out := f.tasks[:0]
	for _, t := range f.tasks {
		if t.ID != id {
			out = append(out, t)
		}
	}
	f.tasks = out
	return nil
}

func botWithTasks(tk TaskStore) *Bot {
	return New(Config{
		API: &fakeAPIRec{}, Store: newFakeStore(), GroupID: -100, MinLevel: notify.LevelInfo,
		Actions: &fakeActions{}, Allowed: []int64{allowedUID}, Redact: fakeScrubber{}, Tasks: tk,
	})
}

// Creating a task parses the schedule, persists it, and confirms with an id.
func TestCmdSchedule_Create(t *testing.T) {
	tk := &fakeTasks{}
	b := botWithTasks(tk)

	out := b.cmdSchedule(context.Background(), "30m :: check the fleet for stuck workers and report")
	require.Contains(t, out, "scheduled")
	require.Len(t, tk.tasks, 1)
	got := tk.tasks[0]
	require.Equal(t, "30m", got.Schedule, "interval canonicalized")
	require.Equal(t, "check the fleet for stuck workers and report", got.Prompt)
	require.NotEmpty(t, got.SessionID, "the task got its own session")
	require.True(t, got.NextRun.After(time.Now()), "next run is in the future")
	require.Contains(t, out, shortTaskID(got.ID))
}

// A cron spec is accepted too.
func TestCmdSchedule_CreateCron(t *testing.T) {
	tk := &fakeTasks{}
	b := botWithTasks(tk)
	out := b.cmdSchedule(context.Background(), "0 8 * * * :: brief me on open issues")
	require.Contains(t, out, "scheduled")
	require.Len(t, tk.tasks, 1)
	require.Equal(t, "0 8 * * *", tk.tasks[0].Schedule)
}

// A malformed schedule or missing prompt returns usage, creates nothing.
func TestCmdSchedule_BadInput(t *testing.T) {
	tk := &fakeTasks{}
	b := botWithTasks(tk)

	require.Contains(t, b.cmdSchedule(context.Background(), "just a prompt no separator"), "usage")
	require.Contains(t, b.cmdSchedule(context.Background(), "nonsense-spec :: do a thing"), "couldn't read the schedule")
	require.Contains(t, b.cmdSchedule(context.Background(), "30m ::   "), "usage")
	require.Empty(t, tk.tasks, "nothing created on bad input")
}

// list / pause / resume / remove operate by short id.
func TestCmdSchedule_Manage(t *testing.T) {
	tk := &fakeTasks{}
	b := botWithTasks(tk)
	b.cmdSchedule(context.Background(), "1h :: watch disk usage")
	id := shortTaskID(tk.tasks[0].ID)

	require.Contains(t, b.cmdSchedule(context.Background(), "list"), "watch disk usage")

	require.Contains(t, b.cmdSchedule(context.Background(), "pause "+id), "paused")
	require.False(t, tk.tasks[0].Enabled)

	require.Contains(t, b.cmdSchedule(context.Background(), "resume "+id), "resumed")
	require.True(t, tk.tasks[0].Enabled)

	require.Contains(t, b.cmdSchedule(context.Background(), "remove "+id), "removed")
	require.Empty(t, tk.tasks)
}

// An unknown id fragment is refused (not a silent no-op).
func TestCmdSchedule_ResolveMiss(t *testing.T) {
	tk := &fakeTasks{}
	b := botWithTasks(tk)
	require.Contains(t, b.cmdSchedule(context.Background(), "remove ZZZZZZ"), "no task matches")
	require.Contains(t, b.cmdSchedule(context.Background(), "pause"), "which task")
}

// Empty / list on a fresh store nudges toward creation.
func TestCmdSchedule_EmptyList(t *testing.T) {
	b := botWithTasks(&fakeTasks{})
	require.Contains(t, b.cmdSchedule(context.Background(), ""), "no scheduled tasks")
	require.Contains(t, b.cmdSchedule(context.Background(), "list"), "no scheduled tasks")
}

// Without a TaskStore wired (bare bot) /schedule reports unavailable, never panics.
func TestCmdSchedule_Unavailable(t *testing.T) {
	b, _, _, _ := newBotWithStore(newFakeCStore())
	require.Contains(t, b.cmdSchedule(context.Background(), "30m :: x"), "aren't available")
}

func TestDeriveTaskName(t *testing.T) {
	require.Equal(t, "check the fleet for stuck workers", deriveTaskName("check the fleet for stuck workers and report back"))
	require.Equal(t, "task", deriveTaskName("   "))
	require.False(t, strings.Contains(deriveTaskName("a\nb\nc"), "\n"), "newlines collapsed")
}
