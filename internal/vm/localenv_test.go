package vm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// newCmd must hand subprocesses an environment with arco's high-blast creds
// stripped (security precondition P1) while preserving benign vars.
func TestNewCmd_ScrubsSpawnEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
	t.Setenv("GITHUB_TOKEN", "ghp_should_not_leak")
	t.Setenv("ARCO_SOCKET", "/run/arco.sock")
	t.Setenv("ARCO_TEST_BENIGN", "keep-me")

	cmd := newCmd(context.Background(), "true")
	require.NotNil(t, cmd.Env, "newCmd must set an explicit (scrubbed) env, not inherit")
	joined := ""
	for _, kv := range cmd.Env {
		joined += kv + "\n"
	}
	require.NotContains(t, joined, "sk-ant-should-not-leak")
	require.NotContains(t, joined, "ghp_should_not_leak")
	require.NotContains(t, joined, "ARCO_SOCKET")
	require.Contains(t, joined, "PATH=", "benign PATH preserved")
}
