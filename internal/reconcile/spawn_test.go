package reconcile

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
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

// A FAILED spawn releases its pool lease immediately (frees the slot for retry),
// rather than holding it until the next sweep's terminal-worker reaper.
func TestSpawn_FailedReleasesLease(t *testing.T) {
	e, s, _ := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.DefaultPool = "p1"
	e.LeaseTTL = time.Hour
	_, err := s.DB().Exec(
		`INSERT INTO provider_pools(id,provider,org,clavis_profile,model_class,max_active,max_starts_per_min,state,cooldown_until,created_at)
		 VALUES('p1','anthropic','','default','',1,100,'ok',NULL,?)`,
		time.Now().UTC().Format(time.RFC3339Nano))
	require.NoError(t, err)

	// bad repo → provision fails in phase 2 → worker failed → lease released
	res, err := e.Spawn(context.Background(), "", "t1", true, filepath.Join(t.TempDir(), "nope"), "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerFailed, res.State)
	n, err := s.Reader().CountActiveLeases("p1")
	require.NoError(t, err)
	require.Equal(t, 0, n, "failed spawn released its lease (slot freed)")

	// the freed slot lets an immediate good spawn acquire (max_active=1)
	repo, _ := localRepo(t)
	r2, err := e.Spawn(context.Background(), "", "t2", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, r2.State)
	n, _ = s.Reader().CountActiveLeases("p1")
	require.Equal(t, 1, n)
}

// A repo-spawned worker's initial task is delivered to its captured PANE
// (AgentRef), not the workspace label — herdr `agent prompt` targets a pane, so
// this is what makes a spawned agent actually start working (live-verified).
func TestSpawn_DeliversInitialTaskToPane(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "do the thing", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)

	w, _ := s.Reader().GetWorker(res.WorkerID)
	prompts := fake.Prompts()
	require.Len(t, prompts, 1, "the initial task is delivered exactly once")
	require.Equal(t, w.AgentRef, prompts[0].Workspace, "delivered to the captured pane (AgentRef)")
	require.NotEqual(t, w.Workspace, prompts[0].Workspace, "NOT targeted at the workspace label")
	require.Contains(t, prompts[0].Text, "do the thing")
	require.Contains(t, prompts[0].Text, "[arco-intent]", "wrapped for delivery confirmation")
}

// promptTarget prefers the captured pane_id (AgentRef), falling back to the
// workspace label only for a worker arco never launched (Fake/legacy path).
func TestPromptTarget_PrefersAgentRefElseWorkspace(t *testing.T) {
	require.Equal(t, "wE:p1", promptTarget(core.Worker{Workspace: "arco_1", AgentRef: "wE:p1"}))
	require.Equal(t, "arco_1", promptTarget(core.Worker{Workspace: "arco_1"}))
}

// A launch that errored but whose agent is alive is adopted RUNNING, but with an
// unknown ref (bound ""), so NO initial task is delivered (delivering by the
// workspace label would target the wrong thing on real herdr). The worker stays
// running + re-promptable.
func TestSpawn_LaunchErrorAdopted_DeliversNoTask(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	fake.LaunchErr = context.DeadlineExceeded
	fake.LaunchAliveOnErr = true
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State, "adopted by liveness")
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Empty(t, w.AgentRef, "ref unknown after launch error")
	require.Empty(t, fake.Prompts(), "no initial task delivered when the pane is unknown")
}

// A failed initial-task delivery is recorded but does NOT fail/park the worker —
// it is running + claimable/re-promptable.
func TestSpawn_InitialTaskDeliveryFailure_StaysRunning(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	fake.PromptErr = errors.New("prompt boom")
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerRunning, w.State, "a failed delivery must not park the worker")
	evs, _ := s.Reader().RecentWorkerEvents(res.WorkerID, 50)
	var sawErr bool
	for _, ev := range evs {
		if ev.Kind == "error" && strings.Contains(ev.Payload, "initial task delivery failed") {
			sawErr = true
		}
	}
	require.True(t, sawErr, "the delivery failure is recorded")
}

// After a repo-spawn, a brain run_again re-prompts the worker at its PANE
// (AgentRef), not the workspace label.
func TestSpawn_ThenRunAgain_TargetsPane(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	repo, _ := localRepo(t)
	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.NotEmpty(t, w.AgentRef)

	before := len(fake.Prompts())
	e.applyStep(context.Background(), res.WorkerID, "cid-ra", core.StepResult{Kind: "run_again", Instruction: "keep going"})
	prompts := fake.Prompts()
	require.Greater(t, len(prompts), before, "run_again delivered a prompt")
	require.Equal(t, w.AgentRef, prompts[len(prompts)-1].Workspace, "run_again targets the pane, not the label")
}

// fakeCreds is a test AgentCredentials that records the profile it was asked for.
type fakeCreds struct {
	env     []string
	err     error
	profile string
}

func (f *fakeCreds) EnvFor(_ context.Context, profile string) ([]string, error) {
	f.profile = profile
	return f.env, f.err
}

func seedPool(t *testing.T, s *ledger.Store, id, clavisProfile string) {
	t.Helper()
	_, err := s.DB().Exec(
		`INSERT INTO provider_pools(id,provider,org,clavis_profile,model_class,max_active,max_starts_per_min,state,cooldown_until,created_at)
		 VALUES(?,'anthropic','',?,'',10,100,'ok',NULL,?)`,
		id, clavisProfile, time.Now().UTC().Format(time.RFC3339Nano))
	require.NoError(t, err)
}

// A worker leased from a pool with a clavis_profile launches with that profile's
// SCOPED creds injected into the launch env (post-scrub) — the provider-pool
// worker-auth model. The resolver is asked for the POOL's profile.
func TestSpawn_InjectsPoolClavisCreds(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.DefaultPool = "p1"
	e.LeaseTTL = time.Hour
	fc := &fakeCreds{env: []string{"ANTHROPIC_AUTH_TOKEN=scoped-tok", "ANTHROPIC_BASE_URL=https://ds"}}
	e.Creds = fc
	seedPool(t, s, "p1", "deepseek-1")
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)
	require.Equal(t, "deepseek-1", fc.profile, "resolved the pool's clavis_profile")
	joined := strings.Join(fake.Launched()[0].Env, " ")
	require.Contains(t, joined, "ANTHROPIC_AUTH_TOKEN=scoped-tok", "scoped creds injected")
	require.Contains(t, joined, "ANTHROPIC_BASE_URL=https://ds")
}

// A pool with NO clavis_profile injects nothing (worker launches credential-less).
func TestSpawn_NoProfileNoCredInjection(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.DefaultPool = "p1"
	e.LeaseTTL = time.Hour
	fc := &fakeCreds{env: []string{"ANTHROPIC_AUTH_TOKEN=scoped-tok"}}
	e.Creds = fc
	seedPool(t, s, "p1", "") // no profile
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)
	require.Empty(t, fc.profile, "EnvFor not called when the pool sets no profile")
	require.NotContains(t, strings.Join(fake.Launched()[0].Env, " "), "scoped-tok")
}

// A credential-resolve error fails the spawn (worker not launched unauthenticated)
// and cleans up the worktree — a pre-launch failure.
func TestSpawn_CredResolveErrorFailsAndCleansUp(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.DefaultPool = "p1"
	e.LeaseTTL = time.Hour
	e.Creds = &fakeCreds{err: errors.New("clavis boom")}
	seedPool(t, s, "p1", "deepseek-1")
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerFailed, res.State, "unauthenticated worker not launched")
	require.Empty(t, fake.Launched(), "no launch on a credential-resolve failure")
	require.NoDirExists(t, filepath.Join(e.ConfigDir, res.WorkerID), "worktree cleaned up (pre-launch failure)")
}
