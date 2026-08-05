// Package daemon wires the ledger, reconcile engine, VMClient, and API server
// into a long-running process listening on a unix socket.
package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/dinhlongviolin1/arco/internal/api"
	"github.com/dinhlongviolin1/arco/internal/config"
	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// Deps are the wired dependencies; Run builds real ones from config unless
// overridden (tests inject a fake VMClient).
type Deps struct {
	VM core.VMClient
}

// Run opens the ledger, migrates, and serves the API on cfg.Socket until ctx is
// cancelled. It closes the listener and store on shutdown.
func Run(ctx context.Context, cfg config.Config, deps Deps) error {
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Socket), 0o700); err != nil {
		return err
	}
	store, err := ledger.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	vmc := deps.VM
	if vmc == nil {
		// TODO(Task S): swap for vm.LocalVMClient (clavis/herdr) once the
		// clavis/herdr contract spike lands. Fake keeps the daemon headless-runnable.
		vmc = vm.NewFake()
	}
	eng := reconcile.New(store, vmc)
	srv := api.New(store, eng)

	// Fresh socket (a stale one from a crash blocks bind).
	_ = os.Remove(cfg.Socket)
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return fmt.Errorf("daemon: listen %s: %w", cfg.Socket, err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		ln.Close()
		_ = os.Remove(cfg.Socket)
		return nil
	case err := <-errCh:
		_ = os.Remove(cfg.Socket)
		return err
	}
}
