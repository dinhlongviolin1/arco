package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoad_TOMLFieldsAndDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
brain_profile  = "deepseek-1"
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)

	require.Equal(t, "deepseek-1", cfg.BrainProfile)
	// omitted socket + brain_model get defaults; brain_model is a CHEAP tier, not opus.
	require.NotEmpty(t, cfg.Socket)
	require.Equal(t, "haiku", cfg.BrainModel)
	require.NotEqual(t, "opus", cfg.BrainModel)
	// pinned operability defaults present.
	require.Equal(t, 4, cfg.MaxBrainCalls)
	require.Equal(t, 30*time.Second, cfg.SweepInterval)
	require.Equal(t, 8, cfg.MaxChildrenPerSession)
}

func TestLoad_MissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	require.NoError(t, err)
	require.Equal(t, "haiku", cfg.BrainModel)
	require.Equal(t, 3, cfg.StallN)
}

func TestLoad_EnvOverride(t *testing.T) {
	t.Setenv("ARCO_BRAIN_MODEL", "sonnet")
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "sonnet", cfg.BrainModel)
}

func TestLoad_EmptyBrainModelInTOMLStillGetsCheapDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.toml")
	require.NoError(t, os.WriteFile(path, []byte(`brain_model = ""`), 0o600))
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "haiku", cfg.BrainModel)
}

func TestLoad_RejectsNonPositiveSweepInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("sweep_interval = 0\n"), 0o600))
	_, err := Load(path)
	require.Error(t, err, "sweep_interval = 0 would panic time.NewTicker; Load must reject it")
	require.Contains(t, err.Error(), "sweep_interval")
}
