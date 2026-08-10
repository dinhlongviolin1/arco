package reconcile

// Security regression: an intake observed_head flows verbatim into git/gh
// command lines downstream (mergeq fetch/merge, civerify's gh api path). A
// worker-supplied head that is not a plausible commit id must be dropped at the
// ObserveWorker boundary — never poison head_commit with an option-injection or
// path-traversal payload.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestApplyEvent_MaliciousHeadDropped(t *testing.T) {
	e, s, _ := newEngine(t)
	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)

	for _, bad := range []string{
		"--upload-pack=touch /tmp/pwn",
		"../../../../etc/passwd",
		"deadbeef; rm -rf /",
		"$(whoami)",
		"main", // a ref name, not a commit id — also rejected (too short-safe, non-hex)
	} {
		require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
			WorkerID: res.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: bad,
		}))
		w, _ := s.Reader().GetWorker(res.WorkerID)
		require.NotEqual(t, bad, w.HeadCommit, "a non-commit-shaped head must never be stored: %q", bad)
		require.NotEqual(t, core.WorkerCompletedCandidate, w.State,
			"a bad head must not count as HEAD-advanced progress: %q", bad)
	}

	// A well-formed commit id still lands (the gate rejects shape, not content).
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: res.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: "a1b2c3d4e5f6",
	}))
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, "a1b2c3d4e5f6", w.HeadCommit)
}
