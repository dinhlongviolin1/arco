package test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/api"
	"github.com/dinhlongviolin1/arco/internal/client"
	"github.com/dinhlongviolin1/arco/internal/config"
	"github.com/dinhlongviolin1/arco/internal/daemon"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// TestEndToEnd_DispatchHookComplete exercises the full headless path over the
// real unix socket + HTTP client: start daemon → dispatch → herdr hook →
// worker reaches completed_candidate → re-POST same event id is a dedup no-op.
func TestEndToEnd_DispatchHookComplete(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(dir, "arco.db")
	cfg.Socket = filepath.Join(dir, "arco.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg, daemon.Deps{VM: vm.NewFake()}) }()

	c := client.New(cfg.Socket)
	waitHealthy(t, c)

	// dispatch → a new session + a running worker
	disp, err := c.Dispatch(ctx, api.DispatchReq{Task: "fix the bug", New: true})
	require.NoError(t, err)
	require.NotEmpty(t, disp.WorkerID)
	require.NotEmpty(t, disp.SessionID)

	// herdr hook: worker went idle and advanced HEAD → completed_candidate
	ev := api.EventReq{
		Source: "herdr:vm0", SourceEventID: "evt-1", Hash: "h1",
		WorkerRef: disp.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: "abc123",
	}
	r, err := c.PostEvent(ctx, ev)
	require.NoError(t, err)
	require.False(t, r.Deduped)

	waitWorkerState(t, c, disp.WorkerID, "completed_candidate")

	// re-POST the same event id → idempotent no-op, state unchanged
	r2, err := c.PostEvent(ctx, ev)
	require.NoError(t, err)
	require.True(t, r2.Deduped)

	ws, err := c.Workers(ctx)
	require.NoError(t, err)
	require.Len(t, ws.Workers, 1)
	require.Equal(t, "completed_candidate", ws.Workers[0].State)

	// graceful shutdown
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

func waitHealthy(t *testing.T, c *client.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h, err := c.Health(context.Background()); err == nil && h.Status == "ok" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon never became healthy")
}

func waitWorkerState(t *testing.T, c *client.Client, id, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ws, err := c.Workers(context.Background())
		require.NoError(t, err)
		for _, w := range ws.Workers {
			if w.ID == id && w.State == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker %s never reached %s", id, want)
}
