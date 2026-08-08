package mergeq_test

// GUIDELINE TESTS — rev7 T3.2 (verification leg 2: in-daemon merge queue).
//
// Pinned surface (package internal/mergeq):
//   - New(s core.Store, cfg Config) *Queue
//   - Config{GitBin string, TestCmd []string} — GitBin "" = "git";
//     TestCmd empty = no test gate, else run in the merge workspace and a
//     non-zero exit kicks the item back.
//   - (*Queue).Enqueue(ctx, workerID string) (string, error) — reads the
//     worker row (worktree, head); the target repo is the clone's `origin`
//     remote. One PENDING item per worker (re-enqueue returns the existing
//     id). Unknown worker → error.
//   - (*Queue).ProcessNext(ctx) (bool, error) — oldest pending item;
//     (false, nil) when the queue is empty. A kickback is an OUTCOME
//     (item kicked + confirm escalation), not an error.
//   - (*Queue).Items(ctx) ([]Item, error) — enqueue (FIFO) order.
//   - Item{ID, WorkerID, Repo, Head, Status string}
//   - StatusPending / StatusMerged / StatusKicked
//
// Pinned semantics:
//   - a clean merge lands the worker's commit on origin's main and appends ONE
//     verification_artifact event (payload mentions "merge"), deduped forever
//     (re-processing never duplicates it).
//   - a conflicting merge or a red TestCmd kicks the item back: origin main
//     is NOT moved, no artifact, one pending `confirm` escalation for the
//     worker (OpenEscalation dedup applies) whose text mentions the merge.
//   - the queue is LEDGER-BACKED: a fresh Queue over the same store (daemon
//     restart) sees the same items and resumes where it left off.
//   - merging never auto-verifies the worker: state stays completed_candidate
//     (the human diff-gate in verify.go remains the only path to
//     completed_verified).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/mergeq"
)

func gitTry(dir string, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	return string(out), err
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitTry(dir, args...)
	require.NoError(t, err, "git %v: %s", args, out)
	return out
}

// newOrigin builds a BARE target repo with one commit on main (non-bare
// targets refuse pushes to the checked-out branch; the bare repo stands in
// for the real remote). Returns the repo path and the base commit sha.
func newOrigin(t *testing.T) (origin, base string) {
	t.Helper()
	origin = filepath.Join(t.TempDir(), "origin.git")
	git(t, t.TempDir(), "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(t.TempDir(), "seed")
	git(t, t.TempDir(), "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-B", "main")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "README.md"), []byte("base\n"), 0o644))
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "init")
	git(t, seed, "push", "-q", "origin", "main")
	return origin, strings.TrimSpace(git(t, seed, "rev-parse", "HEAD"))
}

// workerClone clones origin (the worker's provisioned worktree model) and
// commits one file. Returns the clone path and its HEAD sha.
func workerClone(t *testing.T, origin, file, content string) (dir, head string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "wt")
	git(t, t.TempDir(), "clone", "-q", origin, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "work: "+file)
	return dir, strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

// mainlineCommit advances origin main directly (someone else merged first).
func mainlineCommit(t *testing.T, origin, file, content string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "mainline")
	git(t, t.TempDir(), "clone", "-q", origin, clone)
	require.NoError(t, os.WriteFile(filepath.Join(clone, file), []byte(content), 0o644))
	git(t, clone, "add", ".")
	git(t, clone, "commit", "-q", "-m", "mainline: "+file)
	git(t, clone, "push", "-q", "origin", "main")
}

// originShow reads a file at origin's main tip ("" + false when absent).
func originShow(t *testing.T, origin, path string) (string, bool) {
	t.Helper()
	out, err := gitTry(origin, "show", "main:"+path)
	return out, err == nil
}

func openStore(t *testing.T) *ledger.Store {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "arco.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedCandidate creates a completed_candidate worker whose worktree/head point
// at the given clone — the state a worker is in when it becomes mergeable.
func seedCandidate(t *testing.T, s *ledger.Store, worktree, head, base string) string {
	t.Helper()
	sid, wid := ulid.Make().String(), ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sid, Status: core.SessionOpen, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		if err := tx.CreateWorker(core.Worker{
			ID: wid, OwnerSession: sid, State: core.WorkerStarting,
			Workspace: "arco_" + wid, Worktree: worktree,
			BaseCommit: base, HeadCommit: head, Task: "t", RunReason: "dispatch",
		}); err != nil {
			return err
		}
		if err := tx.TransitionWorker(wid, core.WorkerRunning, 0, core.Event{Kind: "state_change", WorkerID: wid}); err != nil {
			return err
		}
		return tx.TransitionWorker(wid, core.WorkerCompletedCandidate, 1, core.Event{Kind: "state_change", WorkerID: wid})
	}))
	return wid
}

func countMergeArtifacts(t *testing.T, s *ledger.Store, workerID string) int {
	t.Helper()
	evs, err := s.Reader().RecentWorkerEvents(workerID, 100)
	require.NoError(t, err)
	n := 0
	for _, ev := range evs {
		if ev.Kind == "verification_artifact" {
			require.Contains(t, strings.ToLower(ev.Payload), "merge",
				"the artifact says WHAT verified the worker")
			n++
		}
	}
	return n
}

func pendingConfirms(t *testing.T, s *ledger.Store, workerID string) []core.Escalation {
	t.Helper()
	escs, err := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: workerID})
	require.NoError(t, err)
	return escs
}

func TestMergeQ_CleanMergeLandsOnOriginMain(t *testing.T) {
	s := openStore(t)
	origin, base := newOrigin(t)
	wt, head := workerClone(t, origin, "feature.txt", "feature\n")
	wid := seedCandidate(t, s, wt, head, base)
	ctx := context.Background()

	q := mergeq.New(s, mergeq.Config{})
	id, err := q.Enqueue(ctx, wid)
	require.NoError(t, err)
	require.NotEmpty(t, id)

	items, err := q.Items(ctx)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, wid, items[0].WorkerID)
	require.Equal(t, head, items[0].Head)
	require.Equal(t, origin, items[0].Repo, "target repo read from the clone's origin remote")
	require.Equal(t, mergeq.StatusPending, items[0].Status)

	ok, err := q.ProcessNext(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	got, found := originShow(t, origin, "feature.txt")
	require.True(t, found, "the worker's commit landed on origin main")
	require.Equal(t, "feature\n", got)

	items, err = q.Items(ctx)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, mergeq.StatusMerged, items[0].Status)
	require.Equal(t, 1, countMergeArtifacts(t, s, wid))

	// A merge is verification EVIDENCE, not verification: the human diff-gate
	// stays the only path to completed_verified.
	w, err := s.Reader().GetWorker(wid)
	require.NoError(t, err)
	require.Equal(t, core.WorkerCompletedCandidate, w.State)

	// Empty queue → no-op; the artifact is never duplicated.
	ok, err = q.ProcessNext(ctx)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, 1, countMergeArtifacts(t, s, wid))
}

func TestMergeQ_ConflictKicksBackAndLeavesMainUntouched(t *testing.T) {
	s := openStore(t)
	origin, _ := newOrigin(t)
	wt, head := workerClone(t, origin, "README.md", "worker version\n")
	mainlineCommit(t, origin, "README.md", "mainline version\n") // conflicts with the worker's edit
	wid := seedCandidate(t, s, wt, head, "")
	ctx := context.Background()

	q := mergeq.New(s, mergeq.Config{})
	_, err := q.Enqueue(ctx, wid)
	require.NoError(t, err)

	ok, err := q.ProcessNext(ctx)
	require.NoError(t, err, "a kickback is an outcome, not an error")
	require.True(t, ok)

	got, _ := originShow(t, origin, "README.md")
	require.Equal(t, "mainline version\n", got, "a failed merge NEVER moves origin main")

	items, err := q.Items(ctx)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, mergeq.StatusKicked, items[0].Status)
	require.Equal(t, 0, countMergeArtifacts(t, s, wid), "no artifact for a kicked merge")

	escs := pendingConfirms(t, s, wid)
	require.Len(t, escs, 1, "kickback surfaces as ONE pending escalation")
	require.Equal(t, "confirm", escs[0].Kind)
	require.Contains(t, strings.ToLower(escs[0].Action+" "+escs[0].Detail), "merge")
}

func TestMergeQ_RedTestsKickBack(t *testing.T) {
	s := openStore(t)
	origin, base := newOrigin(t)
	wt, head := workerClone(t, origin, "feature.txt", "feature\n") // merges cleanly
	wid := seedCandidate(t, s, wt, head, base)
	ctx := context.Background()

	q := mergeq.New(s, mergeq.Config{TestCmd: []string{"sh", "-c", "exit 1"}})
	_, err := q.Enqueue(ctx, wid)
	require.NoError(t, err)

	ok, err := q.ProcessNext(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	_, found := originShow(t, origin, "feature.txt")
	require.False(t, found, "red tests → nothing lands on origin main")

	items, err := q.Items(ctx)
	require.NoError(t, err)
	require.Equal(t, mergeq.StatusKicked, items[0].Status)
	require.Equal(t, 0, countMergeArtifacts(t, s, wid))
	require.Len(t, pendingConfirms(t, s, wid), 1)
}

func TestMergeQ_FIFOAndSecondMergeRebasesOverFirst(t *testing.T) {
	s := openStore(t)
	origin, base := newOrigin(t)
	wt1, h1 := workerClone(t, origin, "one.txt", "one\n")
	wt2, h2 := workerClone(t, origin, "two.txt", "two\n") // cloned BEFORE w1 merges
	w1 := seedCandidate(t, s, wt1, h1, base)
	w2 := seedCandidate(t, s, wt2, h2, base)
	ctx := context.Background()

	q := mergeq.New(s, mergeq.Config{GitBin: "git"})
	_, err := q.Enqueue(ctx, w1)
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, w2)
	require.NoError(t, err)

	items, err := q.Items(ctx)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, w1, items[0].WorkerID, "FIFO: first enqueued is first")
	require.Equal(t, w2, items[1].WorkerID)

	ok, err := q.ProcessNext(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	items, _ = q.Items(ctx)
	require.Equal(t, mergeq.StatusMerged, items[0].Status)
	require.Equal(t, mergeq.StatusPending, items[1].Status, "ONE item per ProcessNext — strictly serialized")

	// w2's clone is now BEHIND origin main (w1 landed after w2 cloned). The
	// queue integrates non-conflicting work rather than failing the push.
	ok, err = q.ProcessNext(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	items, _ = q.Items(ctx)
	require.Equal(t, mergeq.StatusMerged, items[1].Status)

	_, found1 := originShow(t, origin, "one.txt")
	_, found2 := originShow(t, origin, "two.txt")
	require.True(t, found1 && found2, "both workers' files present at origin main")
}

func TestMergeQ_RestartResumesAndEnqueueDedups(t *testing.T) {
	s := openStore(t)
	origin, base := newOrigin(t)
	wt1, h1 := workerClone(t, origin, "one.txt", "one\n")
	wt2, h2 := workerClone(t, origin, "two.txt", "two\n")
	w1 := seedCandidate(t, s, wt1, h1, base)
	w2 := seedCandidate(t, s, wt2, h2, base)
	ctx := context.Background()

	q := mergeq.New(s, mergeq.Config{})
	id1, err := q.Enqueue(ctx, w1)
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, w2)
	require.NoError(t, err)

	// Re-enqueue while pending: no second item, the existing id comes back.
	id1b, err := q.Enqueue(ctx, w1)
	require.NoError(t, err)
	require.Equal(t, id1, id1b)
	items, err := q.Items(ctx)
	require.NoError(t, err)
	require.Len(t, items, 2, "one PENDING item per worker")

	_, err = q.Enqueue(ctx, "no-such-worker")
	require.Error(t, err, "unknown worker cannot be enqueued")

	ok, err := q.ProcessNext(ctx)
	require.NoError(t, err)
	require.True(t, ok)

	// Daemon restart: a FRESH queue over the same store sees the same items
	// and finishes the job — the queue lives in the ledger, not in memory.
	q2 := mergeq.New(s, mergeq.Config{})
	items, err = q2.Items(ctx)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, mergeq.StatusMerged, items[0].Status)
	require.Equal(t, mergeq.StatusPending, items[1].Status)

	ok, err = q2.ProcessNext(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	_, found := originShow(t, origin, "two.txt")
	require.True(t, found, "the restarted queue completed the pending merge")
}
