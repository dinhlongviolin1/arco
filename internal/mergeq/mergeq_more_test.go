package mergeq_test

// Additional T3.2 tests (beyond the guideline file): the non-bare-target
// denied push, the green test gate, and enqueue guards.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/mergeq"
)

// newNonBareOrigin builds a NON-BARE target repo with main checked out — the
// shape of a real operator repo, where git denies a push to the checked-out
// branch by default (receive.denyCurrentBranch).
func newNonBareOrigin(t *testing.T) (origin, base string) {
	t.Helper()
	origin = filepath.Join(t.TempDir(), "origin")
	git(t, t.TempDir(), "init", "-q", "-b", "main", origin)
	require.NoError(t, os.WriteFile(filepath.Join(origin, "README.md"), []byte("base\n"), 0o644))
	git(t, origin, "add", ".")
	git(t, origin, "commit", "-q", "-m", "init")
	return origin, strings.TrimSpace(git(t, origin, "rev-parse", "HEAD"))
}

// A denied push (non-bare target refusing its checked-out branch) is a
// KICKBACK carrying the git error — never a crash or a half-merged state.
func TestMergeQ_DeniedPushKicksBackWithGitError(t *testing.T) {
	s := openStore(t)
	origin, base := newNonBareOrigin(t)
	wt, head := workerClone(t, origin, "feature.txt", "feature\n")
	wid := seedCandidate(t, s, wt, head, base)
	ctx := context.Background()

	q := mergeq.New(s, mergeq.Config{})
	_, err := q.Enqueue(ctx, wid)
	require.NoError(t, err)

	ok, err := q.ProcessNext(ctx)
	require.NoError(t, err, "a denied push is an outcome, not an error")
	require.True(t, ok)

	require.Equal(t, base, strings.TrimSpace(git(t, origin, "rev-parse", "main")),
		"origin main is unmoved by the denied push")
	items, err := q.Items(ctx)
	require.NoError(t, err)
	require.Equal(t, mergeq.StatusKicked, items[0].Status)
	require.Equal(t, 0, countMergeArtifacts(t, s, wid))

	escs := pendingConfirms(t, s, wid)
	require.Len(t, escs, 1)
	require.Contains(t, strings.ToLower(escs[0].Action+" "+escs[0].Detail), "merge")
	require.Contains(t, escs[0].Detail, "push", "the escalation carries the git push error")
}

// Security regression: a worker-influenced head that is not a commit id must be
// refused BEFORE it reaches `git fetch`/`merge`, where `--upload-pack=<cmd>`
// would be command execution. The head is gated at intake too (defense in
// depth); this pins the git-exec boundary.
func TestMergeQ_MaliciousHeadRefusedBeforeGit(t *testing.T) {
	s := openStore(t)
	origin, base := newNonBareOrigin(t)
	wt, _ := workerClone(t, origin, "feature.txt", "feature\n")
	wid := seedCandidate(t, s, wt, "--upload-pack=touch /tmp/arco-pwn", base)
	ctx := context.Background()

	q := mergeq.New(s, mergeq.Config{})
	_, err := q.Enqueue(ctx, wid)
	require.NoError(t, err)

	ok, err := q.ProcessNext(ctx)
	require.NoError(t, err, "a bad head is a kickback outcome, not an error")
	require.True(t, ok)

	items, err := q.Items(ctx)
	require.NoError(t, err)
	require.Equal(t, mergeq.StatusKicked, items[0].Status, "an invalid head kicks back")
	require.Equal(t, base, strings.TrimSpace(git(t, origin, "rev-parse", "main")), "origin main untouched")
	require.Equal(t, 0, countMergeArtifacts(t, s, wid))
}

// A green TestCmd runs in the integration workspace (the merged tree is
// visible to it) and the merge lands.
func TestMergeQ_GreenTestsRunInWorkspaceAndMerge(t *testing.T) {
	s := openStore(t)
	origin, base := newOrigin(t)
	wt, head := workerClone(t, origin, "feature.txt", "feature\n")
	wid := seedCandidate(t, s, wt, head, base)
	ctx := context.Background()

	// The gate only passes if it sees BOTH the base file and the worker's file.
	q := mergeq.New(s, mergeq.Config{TestCmd: []string{"sh", "-c", "test -f README.md && test -f feature.txt"}})
	_, err := q.Enqueue(ctx, wid)
	require.NoError(t, err)

	ok, err := q.ProcessNext(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	_, found := originShow(t, origin, "feature.txt")
	require.True(t, found)
	items, _ := q.Items(ctx)
	require.Equal(t, mergeq.StatusMerged, items[0].Status)
	require.Equal(t, 1, countMergeArtifacts(t, s, wid))
}

// A worker with no worktree/head (never launched) has nothing to integrate.
func TestMergeQ_EnqueueRefusesWorkerWithoutWorktree(t *testing.T) {
	s := openStore(t)
	sid, wid := ulid.Make().String(), ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sid, Status: core.SessionOpen, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		return tx.CreateWorker(core.Worker{
			ID: wid, OwnerSession: sid, State: core.WorkerStarting,
			Workspace: "arco_" + wid, Task: "t", RunReason: "dispatch",
		})
	}))
	q := mergeq.New(s, mergeq.Config{})
	_, err := q.Enqueue(context.Background(), wid)
	require.Error(t, err)
}

// A kicked worker may be re-enqueued: the kick finalized the old item, so a
// new PENDING item is minted (pending-dedup applies to pending only).
func TestMergeQ_ReEnqueueAfterKickMintsNewItem(t *testing.T) {
	s := openStore(t)
	origin, _ := newOrigin(t)
	wt, head := workerClone(t, origin, "README.md", "worker version\n")
	mainlineCommit(t, origin, "README.md", "mainline version\n")
	wid := seedCandidate(t, s, wt, head, "")
	ctx := context.Background()

	q := mergeq.New(s, mergeq.Config{})
	id1, err := q.Enqueue(ctx, wid)
	require.NoError(t, err)
	ok, err := q.ProcessNext(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	id2, err := q.Enqueue(ctx, wid)
	require.NoError(t, err)
	require.NotEqual(t, id1, id2)
	items, err := q.Items(ctx)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, mergeq.StatusKicked, items[0].Status)
	require.Equal(t, mergeq.StatusPending, items[1].Status)
}
