// Guideline tests for T2.3 leg 1: arco's OWN secrets load from
// $CREDENTIALS_DIRECTORY (the systemd-creds / LoadCredential= model — one file
// per credential, named after the key) with the existing ARCO_* env vars as
// fallback, and TOML last. Files beat env beats TOML: an operator who wired
// LoadCredential= must never be silently overridden by a stale env var.
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeCred(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

// A credential file beats both the env var and the TOML value.
func TestCredsDir_FileBeatsEnvAndToml(t *testing.T) {
	cd := t.TempDir()
	writeCred(t, cd, "intake_secret", "file-intake-secret-0123456789\n") // trailing newline: systemd-creds files often end with one
	t.Setenv("CREDENTIALS_DIRECTORY", cd)
	t.Setenv("ARCO_INTAKE_SECRET", "env-intake-secret-0123456789")

	dir := t.TempDir()
	path := filepath.Join(dir, "arco.toml")
	require.NoError(t, os.WriteFile(path, []byte(
		"intake_secret = \"toml-intake-secret-0123456789\"\n",
	), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "file-intake-secret-0123456789", cfg.IntakeSecret,
		"credential file wins and the trailing newline is trimmed")
}

// Without a credentials dir the existing env-over-TOML behavior is unchanged.
func TestCredsDir_EnvFallbackWhenNoDir(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "") // unset for this test
	t.Setenv("ARCO_INTAKE_SECRET", "env-intake-secret-0123456789")

	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, "env-intake-secret-0123456789", cfg.IntakeSecret)
}

// A credentials dir that exists but LACKS a given credential file must not
// clobber the TOML/env value with empty — a partial LoadCredential= setup is
// normal (only some secrets wired as files).
func TestCredsDir_PerKeyFallback(t *testing.T) {
	cd := t.TempDir() // exists but has no intake_secret file
	t.Setenv("CREDENTIALS_DIRECTORY", cd)

	dir := t.TempDir()
	path := filepath.Join(dir, "arco.toml")
	require.NoError(t, os.WriteFile(path, []byte("intake_secret = \"toml-intake-secret-0123456789\"\n"), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "toml-intake-secret-0123456789", cfg.IntakeSecret,
		"missing cred file → the TOML value survives, not an empty clobber")
}
