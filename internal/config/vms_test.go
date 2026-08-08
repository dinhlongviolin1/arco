package config

// GUIDELINE TEST — rev7 T3.3: the configured VM fleet ([[vms]] blocks) the
// daemon builds the Engine's VM registry from (vm.NewRemote per host).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_VMFleet(t *testing.T) {
	// Default: no fleet — routing stays off, VM names stay labels.
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	require.NoError(t, err)
	require.Empty(t, cfg.VMs)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[[vms]]
name = "vm1"
host = "arco@10.0.0.7"
herdr = "/usr/local/bin/herdr"
socket = "/run/user/1000/herdr/default.sock"

[[vms]]
name = "vm2"
host = "vm2.internal"
`), 0o600))
	cfg, err = Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.VMs, 2)
	require.Equal(t, "vm1", cfg.VMs[0].Name)
	require.Equal(t, "arco@10.0.0.7", cfg.VMs[0].Host)
	require.Equal(t, "/usr/local/bin/herdr", cfg.VMs[0].Herdr)
	require.Equal(t, "/run/user/1000/herdr/default.sock", cfg.VMs[0].Socket,
		"per-VM herdr socket path is part of the fleet config")
	require.Equal(t, "vm2", cfg.VMs[1].Name)
	require.Equal(t, "vm2.internal", cfg.VMs[1].Host)
	require.Empty(t, cfg.VMs[1].Herdr, "herdr bin optional (remote default)")
}
