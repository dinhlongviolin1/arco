package reconcile

// GUIDELINE TESTS — rev7 T3.4: spawn hands each worker ITS OWN derived intake
// key as a file (T2.3 creds-dir contract), never the master and never via env.
//
// Pinned seam: Engine.IntakeMaster string — set by the daemon from
// cfg.IntakeSecret. Empty = unsigned mode, no key file.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/intakekey"
)

func TestSpawn_HandsOffDerivedIntakeKeyAsFile(t *testing.T) {
	e, _, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.IntakeMaster = "master-secret"
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)

	keyPath := filepath.Join(e.ConfigDir, res.WorkerID, "creds", "intake_key")
	got, err := os.ReadFile(keyPath)
	require.NoError(t, err, "worker intake key delivered as a creds-dir file")
	require.Equal(t, intakekey.Derive("master-secret", res.WorkerID), string(got),
		"the file holds the WORKER's derived key")
	fi, err := os.Stat(keyPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	joined := strings.Join(fake.Launched()[0].Env, " ")
	require.Contains(t, joined, "CREDENTIALS_DIRECTORY=", "pointer injected even with no pool creds")
	require.NotContains(t, joined, "master-secret", "master never reaches a worker")
	require.NotContains(t, joined, string(got), "derived key never rides the env/argv")
}

func TestSpawn_NoMasterNoIntakeKeyFile(t *testing.T) {
	e, _, _ := newEngine(t)
	e.ConfigDir = t.TempDir()
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "")
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)
	require.NoFileExists(t, filepath.Join(e.ConfigDir, res.WorkerID, "creds", "intake_key"),
		"unsigned mode: no key material provisioned")
}
