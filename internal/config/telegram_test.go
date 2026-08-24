package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "arco.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// Disabled by default; min_level defaults to info.
func TestLoad_TelegramDefaults(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.False(t, cfg.Telegram.Enabled)
	require.Equal(t, "info", cfg.Telegram.MinLevel)
}

// A valid enabled block parses token, group id, allowlist.
func TestLoad_TelegramFromTOML(t *testing.T) {
	path := writeTOML(t, `
[telegram]
enabled = true
token = "123:abc"
group_id = -1001234567890
allowed_user_ids = [573409113, 42]
min_level = "warn"
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Telegram.Enabled)
	require.Equal(t, "123:abc", cfg.Telegram.Token)
	require.EqualValues(t, -1001234567890, cfg.Telegram.GroupID)
	require.Equal(t, []int64{573409113, 42}, cfg.Telegram.AllowedUserIDs)
	require.Equal(t, "warn", cfg.Telegram.MinLevel)
}

// One active notifier: [telegram] enabled AND notify.urls set is a hard error.
func TestLoad_TelegramAndNotifyMutuallyExclusive(t *testing.T) {
	path := writeTOML(t, `
[notify]
urls = ["ntfy://example.com/t"]

[telegram]
enabled = true
token = "123:abc"
group_id = -100123
`)
	_, err := Load(path)
	require.ErrorContains(t, err, "pick one active notifier")
}

// Enabled without a token fails loudly.
func TestLoad_TelegramEnabledNeedsToken(t *testing.T) {
	path := writeTOML(t, "[telegram]\nenabled = true\ngroup_id = -100123\n")
	_, err := Load(path)
	require.ErrorContains(t, err, "telegram.token")
}

// Enabled without a group id fails loudly.
func TestLoad_TelegramEnabledNeedsGroup(t *testing.T) {
	path := writeTOML(t, "[telegram]\nenabled = true\ntoken = \"123:abc\"\n")
	_, err := Load(path)
	require.ErrorContains(t, err, "group_id")
}

// The token may come from the environment instead of the file.
func TestLoad_TelegramTokenFromEnv(t *testing.T) {
	t.Setenv("ARCO_TELEGRAM_TOKEN", "env:token")
	path := writeTOML(t, "[telegram]\nenabled = true\ngroup_id = -100123\n")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "env:token", cfg.Telegram.Token)
}
