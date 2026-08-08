package config

// GUIDELINE TEST — rev7 T3.1: config gate for CI check-runs polling.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_CICheckRunsGate(t *testing.T) {
	// Default: off. Polling gh from the daemon is opt-in.
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	require.NoError(t, err)
	require.False(t, cfg.CICheckRuns)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("ci_check_runs = true\n"), 0o600))
	cfg, err = Load(path)
	require.NoError(t, err)
	require.True(t, cfg.CICheckRuns)
}
