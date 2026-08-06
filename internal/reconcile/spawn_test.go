package reconcile

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// localRepo builds a temp source repo with one commit; returns dir, head.
func localRepo(t *testing.T) (dir, head string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return string(out)
	}
	run("init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644))
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir, strings.TrimSpace(run("rev-parse", "HEAD"))
}

func TestSpawn_ProvisionsCompilesLaunchesAndCorrelates(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	repo, head := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "implement the feature", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)

	w, err := s.Reader().GetWorker(res.WorkerID)
	require.NoError(t, err)
	require.Equal(t, head, w.BaseCommit, "checked-out base recorded")
	require.NotEmpty(t, w.Worktree)
	require.Equal(t, "pane:"+w.Workspace, w.AgentRef, "backend handle bound for sweep correlation")

	// worktree is a real clone; config is staged OUTSIDE it (B6)
	require.DirExists(t, filepath.Join(w.Worktree, ".git"))
	cfgDir := filepath.Join(e.ConfigDir, res.WorkerID, "cfg")
	require.FileExists(t, filepath.Join(cfgDir, "settings.json"))
	require.FileExists(t, filepath.Join(cfgDir, "managed-settings.json"))
	require.False(t, strings.HasPrefix(cfgDir, w.Worktree), "compiled config must live outside the worktree")

	// launched once, with pinned settings flag + a scrubbed env (no arco secret var)
	require.Len(t, fake.Launched(), 1)
	spec := fake.Launched()[0]
	require.Equal(t, "claude", spec.Kind)
	require.Equal(t, w.Worktree, spec.Workdir)
	require.Contains(t, strings.Join(spec.Args, " "), "--settings "+filepath.Join(cfgDir, "settings.json"))
	for _, kv := range spec.Env {
		require.False(t, strings.HasPrefix(kv, "ARCO_"), "arco config vars scrubbed from launch env: %s", kv)
	}
}

func TestSpawn_BadRepoMarksFailedAndCleansUp(t *testing.T) {
	e, s, _ := newEngine(t)
	e.ConfigDir = t.TempDir()
	res, err := e.Spawn(context.Background(), "", "task", true, filepath.Join(t.TempDir(), "nonexistent-repo"), "")
	require.NoError(t, err) // Spawn returns the durable result, not the launch error
	require.Equal(t, core.WorkerFailed, res.State, "provision failure → worker failed")
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerFailed, w.State)
	// F1: the partial per-worker dir is cleaned up on a pre-launch failure (no leak)
	require.NoDirExists(t, filepath.Join(e.ConfigDir, res.WorkerID), "orphan worktree cleaned up")
}

// F2: a launch that spawned the agent but returned an error must be adopted
// RUNNING via liveness (not left terminal while the agent runs unmanaged), and
// correlated by workspace (ref unrecoverable → AgentRef empty).
func TestSpawn_LaunchErrorButAgentAliveIsAdopted(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	fake.LaunchErr = context.DeadlineExceeded
	fake.LaunchAliveOnErr = true
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State, "half-spawned agent adopted running, not left terminal")
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerRunning, w.State)
	require.Empty(t, w.AgentRef, "ref unrecoverable on launch error → correlate by workspace")
	require.NotEmpty(t, w.Worktree, "worktree still recorded")
}

func TestSpawn_RequiresRepoAndConfigDir(t *testing.T) {
	e, _, _ := newEngine(t)
	_, err := e.Spawn(context.Background(), "", "t", true, "", "")
	require.Error(t, err, "empty repo → use Dispatch")
	e2, _, _ := newEngine(t) // ConfigDir unset
	_, err = e2.Spawn(context.Background(), "", "t", true, "/some/repo", "")
	require.Error(t, err, "missing ConfigDir")
}

// With a DefaultPool configured, Spawn acquires + binds a provider-pool lease in
// the create tx; at capacity a further spawn is rejected (admission before intent).
func TestSpawn_AcquiresPoolLease(t *testing.T) {
	e, s, _ := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.DefaultPool = "p1"
	e.LeaseTTL = time.Hour
	_, err := s.DB().Exec(
		`INSERT INTO provider_pools(id,provider,org,clavis_profile,model_class,max_active,max_starts_per_min,state,cooldown_until,created_at)
		 VALUES('p1','anthropic','','default','',1,100,'ok',NULL,?)`,
		time.Now().UTC().Format(time.RFC3339Nano))
	require.NoError(t, err)
	repo, _ := localRepo(t)

	r1, err := e.Spawn(context.Background(), "", "t1", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, r1.State)
	n, err := s.Reader().CountActiveLeases("p1")
	require.NoError(t, err)
	require.Equal(t, 1, n, "lease acquired + bound to the worker")

	// pool at capacity (max_active=1) → the next spawn is rejected before creating a worker
	_, err = e.Spawn(context.Background(), "", "t2", true, repo, "")
	require.ErrorIs(t, err, core.ErrLeaseRejected)
	n, _ = s.Reader().CountActiveLeases("p1")
	require.Equal(t, 1, n, "rejected spawn admitted nothing (tx rolled back)")
}
