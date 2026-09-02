package features

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/feature"
)

func TestScheduleTool(t *testing.T) {
	now := time.Date(2026, 1, 2, 7, 0, 0, 0, time.UTC)
	var gotName, gotSched, gotPrompt string
	var gotNext time.Time
	f := Schedule(func(_ context.Context, name, sched, prompt string, next time.Time) (string, error) {
		gotName, gotSched, gotPrompt, gotNext = name, sched, prompt, next
		return "TASKID12345", nil
	}, func() time.Time { return now })

	require.Equal(t, feature.BrainAct, f.Tool.Access, "mutating → gated by confirm policy")

	// Cron spec: canonicalized + next fire computed, name derived from prompt.
	out, err := f.Tool.Call(context.Background(), json.RawMessage(`{"schedule":"0 8 * * *","prompt":"brief me on open issues and blockers"}`))
	require.NoError(t, err)
	require.Contains(t, out, "scheduled")
	require.Equal(t, "0 8 * * *", gotSched)
	require.Equal(t, "brief me on open issues and blockers", gotPrompt)
	require.Equal(t, "brief me on open issues and", gotName, "name derived from first 6 words")
	require.Equal(t, time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC), gotNext, "next daily-08:00 fire")

	// Interval spec with explicit name.
	_, err = f.Tool.Call(context.Background(), json.RawMessage(`{"schedule":"30m","prompt":"check fleet","name":"fleet watch"}`))
	require.NoError(t, err)
	require.Equal(t, "30m", gotSched)
	require.Equal(t, "fleet watch", gotName)
	require.Equal(t, now.Add(30*time.Minute), gotNext)
}

func TestScheduleTool_BadInput(t *testing.T) {
	called := false
	f := Schedule(func(context.Context, string, string, string, time.Time) (string, error) {
		called = true
		return "X", nil
	}, nil)

	// Missing prompt → guidance, no create.
	out, err := f.Tool.Call(context.Background(), json.RawMessage(`{"schedule":"30m"}`))
	require.NoError(t, err)
	require.Contains(t, out, "provide")
	require.False(t, called)

	// Bad schedule → guidance, no create.
	out, err = f.Tool.Call(context.Background(), json.RawMessage(`{"schedule":"whenever","prompt":"do a thing"}`))
	require.NoError(t, err)
	require.Contains(t, out, "didn't parse")
	require.False(t, called)
}
