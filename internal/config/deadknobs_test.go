package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Guideline tests (rev7/T1.3): crash_loop_restarts and max_spawns are dead
// knobs (never read by any code path) — they are DELETED, and a config that
// still sets them must fail loudly at Load instead of silently doing nothing.
// Do not weaken these asserts.

func writeToml(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoad_RejectsRemovedKnob_MaxSpawns(t *testing.T) {
	_, err := Load(writeToml(t, "max_spawns = 8\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "max_spawns")
}

func TestLoad_RejectsRemovedKnob_CrashLoopRestarts(t *testing.T) {
	_, err := Load(writeToml(t, "crash_loop_restarts = 5\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "crash_loop_restarts")
}

// stall_n survives (it is now enforced by the sweep) and stays configurable.
func TestLoad_StallNStillConfigurable(t *testing.T) {
	cfg, err := Load(writeToml(t, "stall_n = 7\n"))
	require.NoError(t, err)
	require.Equal(t, 7, cfg.StallN)
}
