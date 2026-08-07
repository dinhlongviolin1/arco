package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Unconfigured notify: no URLs (disabled) and the info default level.
func TestLoad_NotifyDefaults(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.Empty(t, cfg.Notify.URLs)
	require.Equal(t, "info", cfg.Notify.MinLevel)
}

func writeNotifyTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "arco.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// A [notify] block parses urls + min_level.
func TestLoad_NotifyFromTOML(t *testing.T) {
	path := writeNotifyTOML(t, "[notify]\nurls = [\"ntfy://example.com/topic\"]\nmin_level = \"warn\"\n")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, []string{"ntfy://example.com/topic"}, cfg.Notify.URLs)
	require.Equal(t, "warn", cfg.Notify.MinLevel)
}

// An empty min_level is treated as info (the default must survive a bare block).
func TestLoad_NotifyEmptyMinLevelIsInfo(t *testing.T) {
	path := writeNotifyTOML(t, "[notify]\nmin_level = \"\"\n")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "info", cfg.Notify.MinLevel)
}

// An invalid min_level fails Load, naming the key.
func TestLoad_NotifyInvalidMinLevel(t *testing.T) {
	path := writeNotifyTOML(t, "[notify]\nmin_level = \"loud\"\n")
	_, err := Load(path)
	require.ErrorContains(t, err, "notify.min_level")
}
