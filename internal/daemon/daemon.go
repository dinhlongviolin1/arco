// Package daemon wires the ledger, reconcile engine, VMClient, and API server
// into a long-running process listening on a unix socket.
package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/dinhlongviolin1/arco/internal/api"
	"github.com/dinhlongviolin1/arco/internal/config"
	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/herdrsock"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/memory"
	"github.com/dinhlongviolin1/arco/internal/mergeq"
	"github.com/dinhlongviolin1/arco/internal/notify"
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

// vmProbeTimeout bounds each fleet host's boot-time reachability probe (one
// ListAgents over ssh; BatchMode fails fast, this is the hang ceiling).
const vmProbeTimeout = 10 * time.Second

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
	// as root, no git, a world-readable state dir, network intake without a
	// signing secret, or an enabled [sandbox] with no srt binary. arco enforces
	// its half; the operator owns OS-user setup / branch protection.
	pf := preflight.Evaluate(preflight.Gather(filepath.Dir(cfg.DBPath), filepath.Dir(cfg.Socket), cfg.TCPAddr, cfg.IntakeSecret, cfg.Sandbox.Enabled))
	if !pf.OK() {
		return fmt.Errorf("daemon: preflight failed: %v", pf.Failures())
	}
	for _, w := range pf.Failures() { // surface non-fatal warnings on the happy path
		log.Printf("arco: preflight %s", w)
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
			// Fail fast + clear if the herdr binary is missing, rather than a cryptic
			// "exec: herdr: not found" from the first ListAgents in boot Recover.
			bin := cfg.HerdrBin
			if bin == "" {
				bin = "herdr"
			}
			if _, err := exec.LookPath(bin); err != nil {
				return fmt.Errorf("daemon: use_local_vm set but herdr binary %q not found: %w", bin, err)
			}
			vmc = vm.NewLocal(cfg.HerdrBin)
		} else {
			vmc = vm.NewFake()
		}
	}
	eng := reconcile.New(store, vmc)
	eng.MissThreshold = cfg.LivenessMissThreshold
	eng.StallN = cfg.StallN // sweep blocks a running worker after this many no-progress sweeps
	eng.PoolTTL = cfg.PoolTTL
	eng.BrainRate = cfg.PerSessionBrainRate
	eng.MaxChildren = cfg.MaxChildrenPerSession
	eng.RollupInterval = cfg.RollupInterval
	eng.EscalationTimeout = cfg.EscalationTimeout // sweep auto-pauses a worker whose escalation went unanswered
	eng.SelfOpWindow = cfg.SelfOpWindow           // D9 back-off: how long a pane's activity still reads as arco's own echo
	eng.ActivityRestoreAfter = cfg.ActivityRestoreAfter
	eng.DefaultVM = cfg.DefaultVM
	eng.MaxWorkersPerVM = cfg.MaxWorkersPerVM
	// Cross-VM fleet registry (rev7/T3.3): one vm.NewRemote client per [[vms]]
	// entry, so each worker's VM ops (launch/prompt/liveness/heads/kill/diff)
	// route to ITS host over the validated SSH layer. Gated on use_local_vm —
	// Fake/headless mode stays single-client with VM names as labels. A bad
	// definition (empty/duplicate name, empty or '-'-prefixed host, a default_vm
	// with no entry) fails startup: a typo'd fleet must not half-start.
	// VMDef.Socket is stored but RESERVED — the confirmed herdr CLI takes no
	// socket flag/env input (docs/herdr-contract.md; §10).
	if cfg.UseLocalVM && len(cfg.VMs) > 0 {
		reg := make(map[string]core.VMClient, len(cfg.VMs))
		for _, def := range cfg.VMs {
			if def.Name == "" {
				return fmt.Errorf("daemon: [[vms]] entry with empty name (host %q)", def.Host)
			}
			if _, dup := reg[def.Name]; dup {
				return fmt.Errorf("daemon: duplicate [[vms]] name %q", def.Name)
			}
			c, err := vm.NewRemote(def.Host, def.Herdr)
			if err != nil {
				return fmt.Errorf("daemon: vm %q: %w", def.Name, err)
			}
			reg[def.Name] = c
		}
		if cfg.DefaultVM != "" {
			if _, ok := reg[cfg.DefaultVM]; !ok {
				return fmt.Errorf("daemon: default_vm %q has no [[vms]] entry — every dispatch would be refused", cfg.DefaultVM)
			}
		}
		eng.VMs = reg
		// Per-VM reachability preflight: one ListAgents probe per fleet host.
		// LOGGED, not fatal (documented choice, §10): an unreachable host is
		// transient (reboot ordering, network) and its workers are simply
		// unobservable until it recovers — matching the sweep's per-VM posture —
		// whereas the definition errors above can never self-heal and do refuse
		// startup.
		for name, c := range reg {
			pctx, cancel := context.WithTimeout(ctx, vmProbeTimeout)
			if _, err := c.ListAgents(pctx); err != nil {
				log.Printf("arco: preflight: vm %q unreachable (%v) — its workers stay unobservable until it recovers", name, err)
			}
			cancel()
		}
	}
	eng.ConfigDir = filepath.Join(filepath.Dir(cfg.DBPath), "workers") // per-worker worktrees + configs (outside any worktree)
	// Manual memory (USER.md + MEMORY.md) rooted next to the ledger, so the brain
	// prompt and any hand-editing of those files see the SAME tree (T2.4).
	eng.Memory = memory.New(filepath.Join(filepath.Dir(cfg.DBPath), "memory"))
	eng.GitBin = "git"
	eng.DefaultPool = cfg.DefaultPool
	eng.LeaseTTL = cfg.LeaseTTL
	// Push decision cards (rev7/T1.1): no URLs → a no-op sender (disabled). Load
	// validates min_level, so this cannot fail for a loaded config.
	notifyLevel, err := notify.ParseLevel(cfg.Notify.MinLevel)
	if err != nil {
		return fmt.Errorf("daemon: notify: %w", err)
	}
	eng.Notify = notify.New(cfg.Notify.URLs, notifyLevel)
	if cfg.UseLocalVM {
		// A spawned worker authenticates via its pool's clavis profile (scoped
		// creds injected post-scrub at launch) — not arco's inherited creds (P1).
		// Inert unless a pool sets a clavis_profile. Fake/headless deploys skip this.
		eng.Creds = vm.NewClavisCreds("")
		// The local herdr spawns workers under OUR UID — record it on the worker
		// row so the UDS intake can bind events to it via SO_PEERCRED (rev7/T1.6).
		// Fake/cross-VM leave it nil (ungated, as before).
		uid := os.Getuid()
		eng.SpawnUID = &uid
	}
	// Fail LOUD at startup on a misconfigured pool rather than failing every spawn
	// (AcquireLease→GetPool would ErrNotFound per dispatch). NB: leases gate only
	// the repo-spawn path today; the prompt-path Dispatch is uncapped (follow-up).
	if cfg.DefaultPool != "" {
		if _, err := store.Reader().GetPool(cfg.DefaultPool); err != nil {
			return fmt.Errorf("daemon: default_pool %q not found: %w", cfg.DefaultPool, err)
		}
	}
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
	// Spawn hands each worker its DERIVED intake key as a cred file (T3.4); the
	// intake above verifies against the same derivation. The master never
	// reaches a worker.
	eng.IntakeMaster = cfg.IntakeSecret
	// Observability (rev7/T2.5): GET /metrics on the same socket-served mux.
	// The fleet gauges read the ledger at scrape time; the runtime instruments
	// are fed from here (sweep duration) and from the engine seam below (brain
	// calls/tokens, notify failures).
	metrics := api.NewMetrics(store)
	srv.EnableMetrics(metrics)
	eng.Metrics = metrics
	// Verification leg 1 (rev7/T3.1), opt-in: poll completed_candidate workers'
	// GitHub check-runs during the sweep. The real runner shells out to gh
	// inside the worker's worktree (gh resolves owner/repo from its remote).
	if cfg.CICheckRuns {
		eng.CI = reconcile.CICfg{Enabled: true, Runner: reconcile.NewGHRunner("gh")}
	}
	// Verification leg 2 (rev7/T3.2), opt-in: the in-daemon merge queue. Items
	// are enqueued via POST /v1/queue and processed serially from the sweep
	// ticker below — no per-item goroutines; serialized integration is the point.
	var mq *mergeq.Queue
	if cfg.MergeQueue {
		mq = mergeq.New(store, mergeq.Config{TestCmd: cfg.MergeQueueTestCmd})
		srv.EnableMergeQueue(mq)
	}
	// Autonomy earn-out (rev7/T3.5): promotion to brain auto-answers requires a
	// LIVE verification leg — exactly the T3.1/T3.2 opt-ins above — plus the
	// per-class human track record thresholds. With both legs off,
	// VerificationLive stays false and no class ever promotes.
	eng.VerificationLive = cfg.CICheckRuns || cfg.MergeQueue
	eng.EarnOutMinDecisions = cfg.EarnOutMinDecisions
	eng.EarnOutMinAgreement = cfg.EarnOutMinAgreement
	eng.AgentKind = cfg.AgentKind
	eng.AgentArgs = cfg.AgentArgs
	eng.EStopPath = cfg.EStopPath() // `arco pause` sentinel — see reconcile.Paused

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

	// ConnContext captures each conn's peer UID via SO_PEERCRED (unix only), so
	// the intake can bind worker events to the worker's spawn-time UID (rev7/T1.6).
	httpSrv := &http.Server{Handler: srv.Handler(), ConnContext: api.PeerCredConnContext}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	// Authoritative reconcile sweep on a ticker (push is an optimization over it).
	ticker := time.NewTicker(cfg.SweepInterval)
	defer ticker.Stop()
	// Drain the off-write-path brain work AND the periodic sweep goroutine before
	// the store.Close deferred at function entry, on EVERY return path — including
	// the errCh server-error path, where the caller's ctx may still be live and a
	// brain task on eng.bg() would otherwise tx after Close (deepseek review). LIFO
	// over these + the entry store.Close gives sweepCancel → sweepWG.Wait →
	// Exec.Stop → store.Close.
	sweepCtx, sweepCancel := context.WithCancel(ctx)
	var sweepWG sync.WaitGroup
	sweepWG.Add(1)
	defer eng.Exec.Stop()
	defer sweepWG.Wait()
	defer sweepCancel()
	go func() {
		defer sweepWG.Done()
		for {
			select {
			case <-sweepCtx.Done():
				return
			case <-ticker.C:
				start := time.Now()
				_, _ = eng.Sweep(sweepCtx)
				metrics.SweepDone(time.Since(start))
				// Drain the merge queue strictly one item at a time on the sweep
				// cadence (rev7/T3.2) — an error leaves the item pending for the
				// next tick rather than looping hot.
				for mq != nil && !eng.Paused() { // estop: no merges while paused
					ok, err := mq.ProcessNext(sweepCtx)
					if err != nil {
						log.Printf("arco: mergeq: %v", err)
						break
					}
					if !ok {
						break
					}
				}
			}
		}
	}()

	// herdr events.subscribe push subscriber (rev7 D1): a SECOND signal source
	// feeding the same fusion path by AgentRef — the polling sweep above stays
	// untouched as the authoritative fallback. Off by default (herdr_socket "").
	// It joins the sweep's ctx+WaitGroup so shutdown drains it before store.Close.
	if cfg.HerdrSocket != "" {
		hc := &herdrsock.Client{
			SocketPath: cfg.HerdrSocket,
			OnAgentStatus: func(ev herdrsock.AgentStatusEvent) {
				if err := eng.ApplyHerdrStatus(sweepCtx, ev.PaneID, ev.Status); err != nil {
					log.Printf("arco: herdr push: %v", err)
				}
			},
			OnResync: func(obs []core.AgentObs) {
				// Re-seed fusion from the snapshot — push may have missed
				// transitions while the socket was down. Ref-less entries can't
				// correlate to a worker; skip them.
				for _, o := range obs {
					if o.Ref == "" {
						continue
					}
					if err := eng.ApplyHerdrStatus(sweepCtx, o.Ref, o.State); err != nil {
						log.Printf("arco: herdr resync: %v", err)
					}
				}
			},
			OnActivity: func(ev herdrsock.ActivityEvent) {
				// D9 human-activity back-off (T3.6): a human on a worker's pane drops
				// that session auto→assist. Workers are pane-scoped, so workspace/tab
				// focus kinds (empty PaneID) carry no worker to back off from.
				if ev.PaneID == "" {
					return
				}
				if err := eng.ApplyHumanActivity(sweepCtx, ev.PaneID); err != nil {
					log.Printf("arco: activity back-off: %v", err)
				}
			},
			Logf: log.Printf,
		}
		sweepWG.Add(1)
		go func() {
			defer sweepWG.Done()
			_ = hc.Run(sweepCtx) // returns promptly on sweepCancel (shutdown)
		}()
	}

	select {
	case <-ctx.Done():
		// Graceful: stop accepting, drain in-flight requests, THEN let the
		// deferred store.Close run (no write-after-close on an active request).
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sh)
		// Off-write-path brain work + the sweep goroutine are drained before the
		// deferred store.Close by the deferred Exec.Stop/sweepWG.Wait above (ctx is
		// already cancelled, so in-flight clavis calls / txs abort promptly).
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
