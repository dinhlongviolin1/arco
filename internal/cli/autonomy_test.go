package cli

// rev7 T3.5: `arco autonomy` renders the per-class earn-out report as a table
// (the `arco queue list` conventions).

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAutonomyCmd_RendersEveryClass(t *testing.T) {
	socket, _ := startTestDaemon(t)
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--socket", socket, "autonomy"})
	require.NoError(t, root.Execute())

	got := out.String()
	require.Contains(t, got, "verification_live=false", "a bare test engine has no verification leg")
	for _, col := range []string{"CLASS", "AGREE", "TOTAL", "PROMOTES"} {
		require.Contains(t, got, col)
	}
	for _, class := range []string{"clarify", "proceed-confirmation", "scope-change", "resource", "other"} {
		require.True(t, strings.Contains(got, class), "missing class %s in:\n%s", class, got)
	}
}
