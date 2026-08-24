package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// A client disconnect mid-launch cancels the REQUEST ctx. That cancel is exactly
// what errors the launch — and it must NOT also cancel the adoption liveness
// probe, or a live agent gets mislabeled `failed` (skipping BindLaunch) and
// leaks forever (the orphan reaper needs an identity match). The probe runs on a
// bounded e.bg() ctx, so the worker is still correctly adopted `running`.
func TestDispatch_AdoptsLiveAgentDespiteRequestCtxCancel(t *testing.T) {
	e, s, fake := newEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	// Phase 2 (Prompt) cancels the request ctx — simulating a client disconnect
	// right as the launch happens — then reports the launch errored but the agent
	// is alive (spawned-before-error).
	fake.PromptHook = func() { cancel() }
	fake.PromptErr = errors.New("prompt not confirmed (client gone)")
	fake.AliveOnPrompt = true

	res, err := e.Dispatch(ctx, "", "task", true)
	require.NoError(t, err, "phase 1 committed before the cancel; finalize runs on bg")
	e.Exec.Wait()

	w, err := s.Reader().GetWorker(res.WorkerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w.State,
		"a live-but-launch-errored agent must be adopted running via the bg probe, not left failed")
}
