package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// herdr_socket (rev7/T2.1): default empty = push subscriber disabled; TOML
// sets it; ARCO_HERDR_SOCKET overrides TOML (the standard precedence).
func TestLoad_HerdrSocketKnob(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.Empty(t, cfg.HerdrSocket, "default is disabled")

	path := filepath.Join(t.TempDir(), "c.toml")
	require.NoError(t, os.WriteFile(path, []byte(`herdr_socket = "/run/herdr.sock"`), 0o600))
	cfg, err = Load(path)
	require.NoError(t, err)
	require.Equal(t, "/run/herdr.sock", cfg.HerdrSocket)

	t.Setenv("ARCO_HERDR_SOCKET", "/run/env-herdr.sock")
	cfg, err = Load(path)
	require.NoError(t, err)
	require.Equal(t, "/run/env-herdr.sock", cfg.HerdrSocket)
}
