package prompts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The classify trailer must render byte-identical to the old in-code const, or
// the brain's StepResult output contract (and the byte-stable prompt) drift.
func TestClassifyDefault_ByteIdentical(t *testing.T) {
	require.Equal(t,
		`Decide the next step and reply with a JSON StepResult {"kind":"run_again|dispatch|handoff|final_output|question|confirm","instruction":"...","reason":"..."}.`,
		MustText("classify.tmpl"))
}

func TestRollupDefault(t *testing.T) {
	require.Contains(t, MustText("rollup.tmpl"), "Given these sub-worker results")
	require.Contains(t, MustText("rollup.tmpl"), "JSON StepResult")
}

func TestChatRender(t *testing.T) {
	out, err := Render("chat.tmpl", map[string]any{"VMs": "1 — local", "Active": 0, "Pending": 2, "Message": "how many vms?"})
	require.NoError(t, err)
	require.Contains(t, out, "Attached VMs: 1 — local")
	require.Contains(t, out, "Pending decisions: 2")
	require.Contains(t, out, `Operator says: "how many vms?"`)
}

func TestLoadOverride(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollup.tmpl"), []byte("CUSTOM ROLLUP DIRECTIVE\n"), 0o644))
	require.NoError(t, Load(dir))
	require.Equal(t, "CUSTOM ROLLUP DIRECTIVE", MustText("rollup.tmpl"), "an override file replaces the embedded default")
	// embedded defaults not overridden are untouched
	require.Contains(t, MustText("classify.tmpl"), "JSON StepResult")
	// restore embedded for the rest of the suite
	require.NoError(t, Load(""))
	require.Contains(t, MustText("rollup.tmpl"), "Given these sub-worker results")
}

func TestLoad_BadOverrideFailsLoud(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chat.tmpl"), []byte("{{ .Unclosed "), 0o644))
	require.Error(t, Load(dir), "a malformed override must fail Load, not silently fall back")
	require.NoError(t, Load("")) // restore
}

func TestRender_UnknownTemplate(t *testing.T) {
	_, err := Render("nope.tmpl", nil)
	require.Error(t, err)
}
