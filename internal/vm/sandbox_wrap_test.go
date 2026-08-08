// Guideline tests for T2.2's launch-seam half: SandboxWrap is the PURE argv
// transformer that prefixes a worker command with the srt sandbox runtime.
// These tests pin properties, not exact srt flags (srt isn't installed in CI):
//   - disabled ⇒ argv returned unchanged
//   - enabled  ⇒ result starts with the srt binary and ends with the original
//     argv as a contiguous suffix (srt semantics: wrap, never rewrite)
//   - a configured policy path appears among the srt arguments; without one,
//     no empty-string argument is emitted
package vm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// suffixIndex returns the start index of the first contiguous occurrence of
// want inside have, or -1.
func suffixIndex(have, want []string) int {
	if len(want) == 0 {
		return -1
	}
outer:
	for i := 0; i+len(want) <= len(have); i++ {
		for j := range want {
			if have[i+j] != want[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

func TestSandboxWrap_DisabledReturnsArgvUnchanged(t *testing.T) {
	argv := []string{"claude", "--model", "haiku", "-p", "do the thing"}
	got := SandboxWrap(false, "/usr/local/bin/srt", "/etc/arco/policy.json", argv)
	require.Equal(t, argv, got, "disabled sandbox must be a no-op")
}

func TestSandboxWrap_EnabledPrefixesSrtAndKeepsArgvContiguous(t *testing.T) {
	argv := []string{"claude", "--model", "haiku", "-p", "task with spaces & $chars"}
	got := SandboxWrap(true, "/usr/local/bin/srt", "", argv)

	require.NotEmpty(t, got)
	require.Equal(t, "/usr/local/bin/srt", got[0], "srt binary must lead the argv")
	idx := suffixIndex(got, argv)
	require.NotEqual(t, -1, idx, "original argv must survive as a contiguous block: %v", got)
	require.Equal(t, len(got)-len(argv), idx, "original argv must be the trailing block (nothing appended after the command)")
	require.NotContains(t, got, "", "no empty-string argument may be emitted")
}

func TestSandboxWrap_PolicyPathIsPassedThrough(t *testing.T) {
	argv := []string{"claude", "-p", "x"}
	withPolicy := SandboxWrap(true, "srt", "/etc/arco/policy.json", argv)
	require.Contains(t, withPolicy, "/etc/arco/policy.json")

	withoutPolicy := SandboxWrap(true, "srt", "", argv)
	require.NotContains(t, withoutPolicy, "")
	// The two shapes must differ only by the policy flag block, and the empty
	// policy path must not shrink the wrapped command below srt+argv.
	require.Greater(t, len(withPolicy), len(withoutPolicy))
	require.GreaterOrEqual(t, len(withoutPolicy), 1+len(argv))
}

// The wrap must not mutate the caller's slice (spec.Args is reused by the
// launch retry path).
func TestSandboxWrap_DoesNotMutateInput(t *testing.T) {
	argv := []string{"claude", "-p", "x"}
	orig := append([]string(nil), argv...)
	_ = SandboxWrap(true, "srt", "/p.json", argv)
	require.Equal(t, orig, argv)
}
