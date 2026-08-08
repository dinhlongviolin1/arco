// Guideline tests for T3.7: plugin/arco-status is a THIN herdr plugin — a
// manifest plus one script — that surfaces `arco status --json` in the herdr
// UI. Live-verified against herdr 0.7.5: the manifest file is
// `herdr-plugin.toml` with required fields id, name, version,
// min_herdr_version, and `[[actions]]` entries carrying id, title and command
// (an argv ARRAY; a "./"-relative argv[0] resolves against the plugin root).
// `herdr plugin log` captures an action's stdout/stderr/exit code, so the
// script's whole contract is: print the status JSON to stdout, exit 0, and
// find `arco` via PATH — no hardcoded binary locations.
package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
)

const pluginRoot = "../plugin/arco-status"

type herdrPluginManifest struct {
	ID              string   `toml:"id"`
	Name            string   `toml:"name"`
	Version         string   `toml:"version"`
	MinHerdrVersion string   `toml:"min_herdr_version"`
	Platforms       []string `toml:"platforms"`
	Actions         []struct {
		ID      string   `toml:"id"`
		Title   string   `toml:"title"`
		Command []string `toml:"command"`
	} `toml:"actions"`
}

func loadPluginManifest(t *testing.T) herdrPluginManifest {
	t.Helper()
	var m herdrPluginManifest
	_, err := toml.DecodeFile(filepath.Join(pluginRoot, "herdr-plugin.toml"), &m)
	require.NoError(t, err, "plugin manifest must exist and parse as TOML")
	return m
}

// The manifest carries every field herdr 0.7.5 requires to link the plugin,
// and its action script ships alongside it, executable.
func TestHerdrPlugin_ManifestShape(t *testing.T) {
	m := loadPluginManifest(t)
	require.NotEmpty(t, m.ID)
	require.NotEmpty(t, m.Name)
	require.NotEmpty(t, m.Version)
	require.NotEmpty(t, m.MinHerdrVersion, "herdr refuses to link without min_herdr_version")
	require.Contains(t, m.Platforms, "linux", "declare platforms — an undeclared manifest links with a warning")

	require.NotEmpty(t, m.Actions, "at least one action exposes the status view")
	a := m.Actions[0]
	require.NotEmpty(t, a.ID)
	require.NotEmpty(t, a.Title)
	require.NotEmpty(t, a.Command, "command is an argv array")
	require.True(t, strings.HasPrefix(a.Command[0], "./"),
		"argv[0] must be plugin-root-relative so the plugin is relocatable: %q", a.Command[0])

	fi, err := os.Stat(filepath.Join(pluginRoot, a.Command[0]))
	require.NoError(t, err, "action script ships inside the plugin dir")
	require.NotZero(t, fi.Mode().Perm()&0o111, "action script must be executable")
}

// Smoke: running the action's command with a stubbed `arco` on PATH invokes
// `arco status --json` and passes the JSON through to stdout, exit 0.
func TestHerdrPlugin_ScriptSmoke(t *testing.T) {
	m := loadPluginManifest(t)
	require.NotEmpty(t, m.Actions)
	cmdv := m.Actions[0].Command

	const canned = `{"status":"ok","workers":{"running":2},"sessions":{"open":1}}`
	stubDir := t.TempDir()
	argFile := filepath.Join(stubDir, "argv")
	stub := "#!/bin/sh\nprintf '%s' \"$*\" > " + argFile + "\nprintf '%s' '" + canned + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(stubDir, "arco"), []byte(stub), 0o755))
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	abs, err := filepath.Abs(pluginRoot)
	require.NoError(t, err)
	cmd := exec.Command(cmdv[0], cmdv[1:]...)
	cmd.Dir = abs // herdr runs action commands from the plugin root
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		t.Fatalf("script failed: %v\nstderr: %s", err, ee.Stderr)
	}
	require.NoError(t, err)
	require.JSONEq(t, canned, strings.TrimSpace(string(out)),
		"script passes `arco status --json` output through on stdout")

	argv, err := os.ReadFile(argFile)
	require.NoError(t, err, "stub arco was invoked")
	require.Contains(t, string(argv), "status")
	require.Contains(t, string(argv), "--json")
}
