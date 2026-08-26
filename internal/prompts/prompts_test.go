package prompts

import (
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

func TestRender_UnknownTemplate(t *testing.T) {
	_, err := Render("nope.tmpl", nil)
	require.Error(t, err)
}
