package config

// rev7 T3.5: earn-out threshold knobs — pinned defaults (10 / 0.9, like
// self_op_window's defaults-in-config shape) and TOML override.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_EarnOutKnobDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	require.NoError(t, err)
	require.Equal(t, 10, cfg.EarnOutMinDecisions)
	require.InDelta(t, 0.9, cfg.EarnOutMinAgreement, 1e-9)
}

func TestLoad_EarnOutKnobsFromTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path,
		[]byte("earnout_min_decisions = 3\nearnout_min_agreement = 0.75\n"), 0o600))
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, 3, cfg.EarnOutMinDecisions)
	require.InDelta(t, 0.75, cfg.EarnOutMinAgreement, 1e-9)
}
