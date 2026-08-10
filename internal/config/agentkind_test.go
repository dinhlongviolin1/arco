package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_AgentKind(t *testing.T) {
	// Default: claude, no extra args.
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	require.NoError(t, err)
	require.Equal(t, "claude", cfg.AgentKind)
	require.Empty(t, cfg.AgentArgs)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
agent_kind = "codex"
agent_args = ["--profile", "work"]
`), 0o600))
	cfg, err = Load(path)
	require.NoError(t, err)
	require.Equal(t, "codex", cfg.AgentKind)
	require.Equal(t, []string{"--profile", "work"}, cfg.AgentArgs)
}
