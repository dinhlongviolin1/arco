// Guideline tests for T2.3 leg 2 (kills MED-5): a spawned worker's SCOPED
// provider creds must never ride the launch env — herdr's only env mechanism
// is `workspace create --env KEY=VALUE` argv, so anything in LaunchSpec.Env is
// briefly world-readable in /proc/<pid>/cmdline. Instead the resolved creds are
// written one-file-per-key (the systemd credential model) into a 0600-files/
// 0700-dir credentials dir under the worker's PRIVATE per-worker root (outside
// the worktree, so the agent's own repo tools can't commit them), and the env
// carries only the non-secret pointer CREDENTIALS_DIRECTORY=<dir>.
package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
)

type credsStub struct{ env []string }

func (c *credsStub) EnvFor(_ context.Context, _ string) ([]string, error) { return c.env, nil }

func seedPoolT23(t *testing.T, s *ledger.Store, id, clavisProfile string) {
	t.Helper()
	_, err := s.DB().Exec(
		`INSERT INTO provider_pools(id,provider,org,clavis_profile,model_class,max_active,max_starts_per_min,state,cooldown_until,created_at)
		 VALUES(?,'anthropic','',?,'',10,100,'ok',NULL,?)`,
		id, clavisProfile, time.Now().UTC().Format(time.RFC3339Nano))
	require.NoError(t, err)
}

// credsDirOf extracts the CREDENTIALS_DIRECTORY pointer from a launch env,
// requiring exactly one occurrence.
func credsDirOf(t *testing.T, env []string) string {
	t.Helper()
	var dirs []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "CREDENTIALS_DIRECTORY=") {
			dirs = append(dirs, strings.TrimPrefix(kv, "CREDENTIALS_DIRECTORY="))
		}
	}
	require.Len(t, dirs, 1, "exactly one CREDENTIALS_DIRECTORY pointer in the launch env")
	return dirs[0]
}

// The MED-5 regression pin: no secret VALUE in the launch env (hence never on
// herdr argv); creds arrive as 0600 files in a 0700 dir outside the worktree.
func TestSpawnCreds_FileHandoffNotEnv(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.DefaultPool = "p1"
	e.LeaseTTL = time.Hour
	e.Creds = &credsStub{env: []string{
		"ANTHROPIC_AUTH_TOKEN=sk-scoped-123",
		"ANTHROPIC_BASE_URL=https://ds.example",
	}}
	seedPoolT23(t, s, "p1", "deepseek-1")
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)

	spec := fake.Launched()[0]
	joined := strings.Join(spec.Env, " ")
	require.NotContains(t, joined, "sk-scoped-123", "secret value must NOT ride the launch env/argv (MED-5)")
	require.NotContains(t, joined, "ANTHROPIC_AUTH_TOKEN=", "not even an empty assignment of the secret key")

	credDir := credsDirOf(t, spec.Env)
	workerRoot := filepath.Join(e.ConfigDir, res.WorkerID)
	require.True(t, strings.HasPrefix(credDir, workerRoot+string(filepath.Separator)),
		"cred dir lives under the worker's private root: %s", credDir)
	require.False(t, strings.HasPrefix(credDir, filepath.Join(workerRoot, "worktree")+string(filepath.Separator)),
		"cred dir must be OUTSIDE the worktree")

	di, err := os.Stat(credDir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), di.Mode().Perm(), "cred dir is 0700")

	tok, err := os.ReadFile(filepath.Join(credDir, "ANTHROPIC_AUTH_TOKEN"))
	require.NoError(t, err)
	require.Equal(t, "sk-scoped-123", string(tok), "one file per key, exact value bytes")
	fi, err := os.Stat(filepath.Join(credDir, "ANTHROPIC_AUTH_TOKEN"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "cred file is 0600")

	url, err := os.ReadFile(filepath.Join(credDir, "ANTHROPIC_BASE_URL"))
	require.NoError(t, err)
	require.Equal(t, "https://ds.example", string(url), "every resolver entry is file-handed, not just tokens")
}

// No resolved creds ⇒ no pointer var and no cred files: a credential-less
// worker's env must not advertise a directory that doesn't exist.
func TestSpawnCreds_NoProfileNoPointer(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.DefaultPool = "p1"
	e.LeaseTTL = time.Hour
	e.Creds = &credsStub{env: []string{"ANTHROPIC_AUTH_TOKEN=sk-scoped-123"}}
	seedPoolT23(t, s, "p1", "") // pool without a clavis profile → resolver unused

	repo, _ := localRepo(t)
	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)
	require.NotContains(t, strings.Join(fake.Launched()[0].Env, " "), "CREDENTIALS_DIRECTORY=")
}
