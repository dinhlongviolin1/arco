package reconcile

// Regression (rev20 review #1, found by 5 agents): the phase-3 finalize must
// commit on the daemon's background context, NOT the request context — a client
// that disconnects mid-launch must never leave the worker stranded in
// `starting` (the steady-state sweep skips starting; only boot Recover resolves
// it, so a runtime wedge holds a pool lease + an unmanaged agent forever).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestDispatch_ClientDisconnectStillFinalizes(t *testing.T) {
	e, s, fake := newEngine(t)
	e.BgCtx = context.Background() // the daemon's long-lived ctx (survives the request)

	// The request context cancels the instant phase 2 (Prompt) runs — exactly a
	// client Ctrl-C / disconnect after the durable intent committed.
	ctx, cancel := context.WithCancel(context.Background())
	fake.PromptHook = cancel

	res, err := e.Dispatch(ctx, "", "task", true)
	require.NoError(t, err, "a mid-launch disconnect must not fail the dispatch")
	require.NotEmpty(t, res.WorkerID)

	w, err := s.Reader().GetWorker(res.WorkerID)
	require.NoError(t, err)
	require.NotEqual(t, core.WorkerStarting, w.State,
		"worker must be finalized (running), never stranded in starting, despite the cancelled request ctx")
	require.Equal(t, core.WorkerRunning, w.State)
}
