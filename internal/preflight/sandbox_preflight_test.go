// Guideline tests for T2.2's preflight half: when the sandbox is ENABLED the
// daemon must refuse to start without the srt binary (a silently-unsandboxed
// worker is worse than a refused boot); when disabled the check never blocks.
// Pins the Probe fields (SandboxEnabled, SrtPath) and the check name.
package preflight

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// basePassingProbe is a probe that passes every pre-existing critical check,
// so these tests isolate the sandbox check.
func basePassingProbe(t *testing.T) Probe {
	t.Helper()
	return Probe{
		Euid:    1000,
		GitPath: "/usr/bin/git",
	}
}

func findCheck(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not in report: %+v", name, r.Checks)
	return Check{}
}

// Sandbox enabled + srt missing ⇒ CRITICAL failure that blocks startup.
func TestPreflight_SandboxEnabledWithoutSrtFails(t *testing.T) {
	p := basePassingProbe(t)
	p.SandboxEnabled = true
	p.SrtPath = "" // not found on PATH

	r := Evaluate(p)
	c := findCheck(t, r, "sandbox_srt_present")
	require.True(t, c.Critical, "an enabled-but-toothless sandbox must block startup")
	require.False(t, c.Pass)
	require.False(t, r.OK())
}

// Sandbox enabled + srt resolved ⇒ the check passes.
func TestPreflight_SandboxEnabledWithSrtPasses(t *testing.T) {
	p := basePassingProbe(t)
	p.SandboxEnabled = true
	p.SrtPath = "/usr/local/bin/srt"

	r := Evaluate(p)
	c := findCheck(t, r, "sandbox_srt_present")
	require.True(t, c.Pass)
	require.True(t, r.OK())
}

// Sandbox disabled ⇒ srt's absence must NOT fail the report (off by default
// means zero new install burden for operators who never opt in).
func TestPreflight_SandboxDisabledIgnoresSrt(t *testing.T) {
	p := basePassingProbe(t)
	p.SandboxEnabled = false
	p.SrtPath = ""

	r := Evaluate(p)
	require.True(t, r.OK(), "disabled sandbox must not block startup: %v", r.Failures())
}
