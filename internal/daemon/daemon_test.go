package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
