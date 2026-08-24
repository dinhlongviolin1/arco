package vm

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetryablePaneErr(t *testing.T) {
	require.True(t, retryablePaneErr(errors.New(`{"code":"agent_pane_busy","message":"agent target pane wM:p1 is not an available shell"}`)))
	require.True(t, retryablePaneErr(errors.New("agent_pane_busy")))
	require.True(t, retryablePaneErr(errors.New("pane wX:p2 is not an available shell")))
	require.False(t, retryablePaneErr(errors.New("invalid_agent_name")), "a real config error must NOT be retried")
	require.False(t, retryablePaneErr(nil))
}
