// Added-by-T2.2 tests for the GATHER half of the sandbox check: the guideline
// tests pin the pure Evaluate decision over an injected Probe, so these cover
// the OS-facing part — that Gather carries the enabled flag through and resolves
// srt off PATH exactly like it resolves git.
package preflight

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGather_ResolvesSrtFromPath(t *testing.T) {
	binDir := t.TempDir()
	srt := filepath.Join(binDir, "srt")
	require.NoError(t, os.WriteFile(srt, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", binDir)

	p := Gather(t.TempDir(), t.TempDir(), "", "", true)
	require.True(t, p.SandboxEnabled, "the enabled flag must reach the probe")
	require.Equal(t, srt, p.SrtPath)
	require.True(t, findByName(t, Evaluate(p), "sandbox_srt_present").Pass)
}

func TestGather_MissingSrtLeavesPathEmptyAndBlocksWhenEnabled(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // an empty PATH dir: no srt (and no git)

	p := Gather(t.TempDir(), t.TempDir(), "", "", true)
	require.Empty(t, p.SrtPath)
	c := findByName(t, Evaluate(p), "sandbox_srt_present")
	require.True(t, c.Critical)
	require.False(t, c.Pass)

	// Same box, sandbox not opted in: srt's absence is nobody's problem.
	p.SandboxEnabled = false
	require.True(t, findByName(t, Evaluate(p), "sandbox_srt_present").Pass)
}

func findByName(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not in report", name)
	return Check{}
}
