// Guideline tests for T2.2's config surface: an optional [sandbox] section
// (enabled, policy_path) that is OFF by default. These pin the public shape —
// cfg.Sandbox.Enabled / cfg.Sandbox.PolicyPath — and the default-off posture.
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The sandbox is opt-in: defaults and an empty config file both leave it off.
func TestSandbox_OffByDefault(t *testing.T) {
	d := Defaults()
	require.False(t, d.Sandbox.Enabled, "sandbox must be OFF by default")
	require.Empty(t, d.Sandbox.PolicyPath)

	cfg, err := Load("") // no file at all
	require.NoError(t, err)
	require.False(t, cfg.Sandbox.Enabled)
}

// A TOML [sandbox] section with enabled + policy_path parses into the struct.
func TestSandbox_TomlParses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arco.toml")
	require.NoError(t, os.WriteFile(path, []byte(
		"[sandbox]\nenabled = true\npolicy_path = \"/etc/arco/policy.json\"\n",
	), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Sandbox.Enabled)
	require.Equal(t, "/etc/arco/policy.json", cfg.Sandbox.PolicyPath)
}

// A config that enables the sandbox but sets no policy_path still loads (the
// wrapper runs srt with its built-in default policy); Load must not invent a
// hard requirement the preflight layer doesn't have.
func TestSandbox_EnabledWithoutPolicyLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arco.toml")
	require.NoError(t, os.WriteFile(path, []byte("[sandbox]\nenabled = true\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Sandbox.Enabled)
	require.Empty(t, cfg.Sandbox.PolicyPath)
}
