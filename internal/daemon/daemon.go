// Package daemon wires the ledger, reconcile engine, VMClient, and API server
// into a long-running process listening on a unix socket.
package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dinhlongviolin1/arco/internal/api"
	"github.com/dinhlongviolin1/arco/internal/config"
	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/preflight"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/redact"
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

	// Security preflight (PASS-3): refuse to start in an unsafe posture — running
	// as root, no git, a world-readable state dir, or network intake without a
	// signing secret. arco enforces its half; the operator owns OS-user setup /
	// branch protection.
	if pf := preflight.Evaluate(preflight.Gather(filepath.Dir(cfg.DBPath), cfg.TCPAddr, cfg.IntakeSecret)); !pf.OK() {
		return fmt.Errorf("daemon: preflight failed: %v", pf.Failures())
	}

	// Single-instance guard: an exclusive advisory lock on the DB. Two daemons
	// on one DB file would each have their own single-writer mutex, breaking the
	// single-writer invariant (only busy_timeout would mitigate). Refuse to start.
	unlock, err := lockDB(cfg.DBPath)
	if err != nil {
		return err
	}
	defer unlock()

	store, err := ledger.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	store.SetScrubber(redact.New()) // write-time secret redaction (B4)
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	vmc := deps.VM
	if vmc == nil {
		// Default Fake keeps the daemon headless-runnable. Opt in to the real
		// herdr LocalVMClient via use_local_vm / ARCO_LOCAL_VM once the Task-S
		// spike has confirmed herdr's `agent list --json` schema on this host.
		if cfg.UseLocalVM {
			vmc = vm.NewLocal(cfg.HerdrBin)
		} else {
			vmc = vm.NewFake()
		}
	}
	eng := reconcile.New(store, vmc)
	eng.MissThreshold = cfg.LivenessMissThreshold
	eng.PoolTTL = cfg.PoolTTL
	eng.BrainRate = cfg.PerSessionBrainRate
	eng.MaxChildren = cfg.MaxChildrenPerSession
	eng.RollupInterval = cfg.RollupInterval
	eng.Exec = reconcile.NewExec(cfg.MaxBrainCalls)
	eng.BgCtx = ctx // off-write-path brain work observes daemon shutdown
	// Enable the short-lived decision brain only when a profile is configured;
	// otherwise the reconciler stays deterministic-only (ambiguous states wait
	// for the next signal rather than a failing clavis call).
	eng.Redact = redact.New() // scrub the brain prompt before it leaves for the LLM
	if cfg.BrainProfile != "" {
		eng.Brain = reconcile.BrainCfg{Enabled: true, Profile: cfg.BrainProfile, Model: cfg.BrainModel}
	}
	// Network-exposed intake must be signed — enforced by the preflight
	// tcp_intake_signed check above.
	//
	// SCOPE WARNING for whoever wires TCP: the HMAC gate covers ONLY POST
	// /v1/events. dispatch / verify / escalations{answer,confirm} are mutating and
	// UNAUTHENTICATED (they rely on the local unix socket's 0700 dir trust + the
	// local CLI posting unsigned). Do NOT bind cfg.TCPAddr onto srv.Handler()
	// as-is — serve only /v1/events (+ read-only GETs) over TCP, or gate every
	// mutating route behind the same HMAC (and teach the CLI to sign), before any
	// TCP listener is added (opus review, MED latent gap).
	srv := api.New(store, eng)
	srv.SetIntakeSecret(cfg.IntakeSecret)

	// Boot recovery (survive-and-reconcile) before we accept traffic.
	if err := eng.Recover(ctx); err != nil {
		return fmt.Errorf("daemon: boot recovery: %w", err)
	}

	// Fresh socket (a stale one from a crash blocks bind).
	_ = os.Remove(cfg.Socket)
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return fmt.Errorf("daemon: listen %s: %w", cfg.Socket, err)
	}

	httpSrv := &http.Server{Handler: srv.Handler()}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	// Authoritative reconcile sweep on a ticker (push is an optimization over it).
	ticker := time.NewTicker(cfg.SweepInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = eng.Sweep(ctx)
			}
		}
	}()

	select {
	case <-ctx.Done():
		// Graceful: stop accepting, drain in-flight requests, THEN let the
		// deferred store.Close run (no write-after-close on an active request).
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sh)
		// Drain off-write-path brain work BEFORE the deferred store.Close, so no
		// async tx runs after the DB is closed (ctx is already cancelled, so
		// in-flight clavis calls / txs abort promptly).
		eng.Exec.Stop()
		_ = os.Remove(cfg.Socket)
		return nil
	case err := <-errCh:
		_ = os.Remove(cfg.Socket)
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// lockDB takes an exclusive, non-blocking advisory lock on a lockfile next to
// the DB, returning an unlock func. It fails if another daemon holds it.
func lockDB(dbPath string) (func(), error) {
	lockPath := dbPath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("daemon: another arco instance holds %s (%w)", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
