// Added-by-T2.2 tests for the [sandbox] ENV overrides (ARCO_SANDBOX /
// ARCO_SANDBOX_POLICY). The guideline tests cover the TOML + default-off
// surface; these cover the env leg, including its deliberate one-way shape.
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSandbox_EnvEnablesAndSetsPolicy(t *testing.T) {
	t.Setenv("ARCO_SANDBOX", "1")
	t.Setenv("ARCO_SANDBOX_POLICY", "/etc/arco/env-policy.json")

	cfg, err := Load("")
	require.NoError(t, err)
	require.True(t, cfg.Sandbox.Enabled)
	require.Equal(t, "/etc/arco/env-policy.json", cfg.Sandbox.PolicyPath)

	t.Setenv("ARCO_SANDBOX", "true")
	cfg, err = Load("")
	require.NoError(t, err)
	require.True(t, cfg.Sandbox.Enabled, `"true" must be accepted alongside "1"`)
}

// The env override can only turn the sandbox ON: a stray ARCO_SANDBOX=0 must not
// silently un-cage a fleet whose config file opted in.
func TestSandbox_EnvNeverDisablesConfiguredSandbox(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arco.toml")
	require.NoError(t, os.WriteFile(path, []byte("[sandbox]\nenabled = true\n"), 0o600))

	t.Setenv("ARCO_SANDBOX", "0")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Sandbox.Enabled)
}
