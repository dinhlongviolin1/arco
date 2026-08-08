package vm

// CROSS-HOST routing smoke (rev7/T3.3), env-gated like ssh_integration_test.go:
//
//	ARCO_TEST_SSH_HOST=vm1 go test ./internal/vm/ -run Integration -v
//
// It drives a REAL Engine dispatch through the named-VM registry to a REAL
// remote client (fake herdr on the remote host): the launch prompt must land on
// the remote host byte-exact while the daemon-local client sees nothing. This is
// the live proof that routing — not just the transport — crosses hosts.
// (Importing reconcile here is test-only and cycle-free: reconcile does not
// import vm outside its tests.)

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
)

func TestIntegration_CrossVMRoutingDispatch(t *testing.T) {
	dir := xhostDir(t)
	c := xhostSetup(t, dir)
	ctx := context.Background()

	s, err := ledger.Open(filepath.Join(t.TempDir(), "x.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(ctx))
	local := NewFake()
	eng := reconcile.New(s, local)
	t.Cleanup(func() { eng.Exec.Stop(); s.Close() })
	eng.VMs = map[string]core.VMClient{"vm1": c}
	eng.DefaultVM = "vm1"

	const task = "cross-vm routing smoke: it's $(a) `b`"
	res, err := eng.Dispatch(ctx, "", task, true)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)

	w, err := s.Reader().GetWorker(res.WorkerID)
	require.NoError(t, err)
	require.Equal(t, "vm1", w.VM)

	// The remote fake herdr dumped its argv NUL-separated: the prompt must have
	// reached the REMOTE host byte-exact, addressed at the worker's workspace.
	out, err := c.cmd(ctx, "cat", dir+"/prompt-dump.bin").Output()
	require.NoError(t, err, "read back remote argv dump")
	want := strings.Join([]string{"agent", "prompt", w.Workspace, task}, "\x00") + "\x00"
	require.Equal(t, want, string(out), "dispatch prompt routed to the assigned VM byte-exact")

	require.Empty(t, local.Prompts(), "nothing prompted on the daemon-local client")
	require.Empty(t, local.Launched())
}
