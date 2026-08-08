package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/api"
	"github.com/dinhlongviolin1/arco/internal/client"
	"github.com/dinhlongviolin1/arco/internal/config"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// TestSingleInstanceLock: a second daemon on the same DB must refuse to start,
// so the single-writer invariant holds per-file, not just per-process.
func TestSingleInstanceLock(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(dir, "arco.db")
	cfg.Socket = filepath.Join(dir, "arco.sock")
	cfg.SweepInterval = time.Hour // don't sweep during the test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, Deps{VM: vm.NewFake()}) }()

	c := client.New(cfg.Socket)
	require.Eventually(t, func() bool {
		h, err := c.Health(context.Background())
		return err == nil && h.Status == "ok"
	}, 5*time.Second, 20*time.Millisecond)

	// a second daemon on the SAME db (different socket) must fail on the lock
	cfg2 := cfg
	cfg2.Socket = filepath.Join(dir, "arco2.sock")
	err := Run(ctx, cfg2, Deps{VM: vm.NewFake()})
	require.Error(t, err)
	require.Contains(t, err.Error(), "another arco instance")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("first daemon did not shut down")
	}
}

// TestGracefulShutdown: Run returns nil on ctx cancel (graceful drain).
func TestGracefulShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(dir, "arco.db")
	cfg.Socket = filepath.Join(dir, "arco.sock")
	cfg.SweepInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, Deps{VM: vm.NewFake()}) }()

	c := client.New(cfg.Socket)
	require.Eventually(t, func() bool {
		h, err := c.Health(context.Background())
		return err == nil && h.Status == "ok"
	}, 5*time.Second, 20*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not shut down gracefully")
	}
}

// MED-6: with a shared intake secret (P4), the LOCAL hook bridge must still work
// — the client signs /v1/events; an unsigned poster is 401'd.
func TestSignedIntake_LocalHookWorksUnderP4(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(dir, "arco.db")
	cfg.Socket = filepath.Join(dir, "arco.sock")
	cfg.SweepInterval = time.Hour
	cfg.IntakeSecret = "topsecret-shared-key-1234567890"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, Deps{VM: vm.NewFake()}) }()

	signed := client.New(cfg.Socket)
	signed.SetIntakeSecret(cfg.IntakeSecret)
	require.Eventually(t, func() bool {
		h, err := signed.Health(context.Background())
		return err == nil && h.Status == "ok"
	}, 5*time.Second, 20*time.Millisecond)

	// The client derives the worker's key from the master (T3.4), so the signed
	// local hook is accepted for a real worker.
	d, err := signed.Dispatch(context.Background(), api.DispatchReq{Task: "x", New: true})
	require.NoError(t, err)
	_, err = signed.PostEvent(context.Background(), api.EventReq{WorkerRef: d.WorkerID, SourceEventID: "e1"})
	require.NoError(t, err, "a signed local event is accepted under P4")

	// Unknown worker_ref in signed mode → 401 (no worker, no derivable key; T3.4).
	_, err = signed.PostEvent(context.Background(), api.EventReq{WorkerRef: "unknown", SourceEventID: "e2"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "401")

	// unsigned client → 401
	unsigned := client.New(cfg.Socket)
	_, err = unsigned.PostEvent(context.Background(), api.EventReq{WorkerRef: d.WorkerID, SourceEventID: "e3"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "401", "an unsigned event is rejected under P4")

	cancel()
	<-done
}
