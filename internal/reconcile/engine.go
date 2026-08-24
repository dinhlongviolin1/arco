// Package reconcile is the application layer: it wires the ledger, the VMClient,
// and the brain into the supervise loop. Dispatch is crash-safe
// (intent→execute→result); event application is deterministic via fusion, with
// the brain reserved for genuinely ambiguous states (off the write path, via
// per-worker Exec).
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/brain"
	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/fusion"
	"github.com/dinhlongviolin1/arco/internal/memory"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// BrainCfg configures the short-lived decision brain. Enabled=false (default)
// keeps the reconciler deterministic-only (no brain calls); tests and the daemon
// enable it and inject a Runner.
type BrainCfg struct {
	Enabled bool
	Profile string
	Model   string
	Runner  brain.Runner // injected; nil → clavis DefaultRunner
}

// Metrics is what the engine reports to an observability sink. Deliberately
// tiny and value-only: never a prompt, a task, or anything an operator could
// not safely publish on a scrape endpoint.
type Metrics interface {
	// BrainCall records one brain invocation with its approximate token cost.
	BrainCall(tokens int)
	// NotifyFailure records one failed decision-card push.
	NotifyFailure()
}

// meterBrainCall / meterNotifyFailure are the nil-safe call sites for the
// optional Metrics seam — instrumentation must never be a nil-deref hazard on
// a path that supervises real workers.
func (e *Engine) meterBrainCall(tokens int) {
	if e.Metrics != nil {
		e.Metrics.BrainCall(tokens)
	}
}

func (e *Engine) meterNotifyFailure() {
	if e.Metrics != nil {
		e.Metrics.NotifyFailure()
	}
}

// approxTokens estimates a brain call's token cost from its prompt and raw
// response. clavis is a plain-text CLI with no usage surface, so ~4 bytes per
// token is the best available signal; see the arco_brain_tokens_total HELP.
func approxTokens(prompt, raw string) int { return (len(prompt) + len(raw)) / 4 }

// Engine holds the dependencies the supervise loop needs.
type Engine struct {
	Store core.Store
	VM    core.VMClient
	Exec  *Exec // per-worker serialized off-write-path work
	Brain BrainCfg
	// BgCtx is the long-lived context for off-write-path (Exec) work — NOT a
	// request ctx (which would cancel when an intake HTTP request returns). The
	// daemon sets it to a ctx it cancels on shutdown; nil → context.Background().
	BgCtx context.Context
	// Redact scrubs the brain prompt before it leaves for a third-party LLM (the
	// biggest exfil surface, build-guide B4). nil → no scrub.
	Redact core.Scrubber

	// Memory is the manual-memory store whose always-hot tier-1/2 (USER.md +
	// MEMORY.md) is folded into the brain decision prompt (T2.4). Reads are
	// best-effort: nil, or a dir that doesn't exist, simply contributes nothing —
	// it can never fail a classification. The loaded text flows through Redact
	// with the rest of the prompt (memory is operator-authored and may hold
	// secrets).
	Memory *memory.Store

	// Notify receives push decision cards (escalation opened/answered/expired,
	// worker lost/failed/verified). nil = disabled. Emits are POST-COMMIT and
	// best-effort: a send error is logged, never fails the reconcile path.
	Notify notify.Sender

	// MissThreshold is how many consecutive sweeps a worker may be unobserved
	// before it is finalized (suspect_missing → lost / completed_candidate).
	MissThreshold int

	// StallN is how many consecutive sweeps an ALIVE worker may be observed with
	// an UNCHANGED git HEAD (no progress) before it is transitioned running→blocked
	// with a stall question escalation (0 = disabled). Set from config by the daemon.
	StallN int

	// PoolTTL is how long a released worker may sit unclaimed in the pool before
	// the sweep pauses it (0 = reaping disabled). Set from config by the daemon.
	PoolTTL time.Duration

	// BrainRate caps brain calls per session per minute (0 = unlimited). Guards
	// clavis/provider rate quotas against a single session storming the brain.
	BrainRate int

	// EscalationTimeout auto-resolves a pending escalation the operator never
	// answered: after this age the sweep expires it and PAUSES the waiting worker
	// (build-guide "timeout → auto-pause"), so a worker can't wait on a human
	// forever. 0 = disabled.
	EscalationTimeout time.Duration

	// BrainIntentGrace is how old a dangling brain_intent must be before the sweep
	// re-drives it (a classification lost to a crash in the off-write-path call
	// window). It is floored to ≥ 2× the brain-call timeout so a still-in-flight
	// call is never re-driven; 0 or a sub-floor value → defaultBrainIntentGrace.
	BrainIntentGrace time.Duration

	// MaxChildren caps the number of active workers a session may own — the
	// delegation fan-in cap (0 = unlimited). Set from config by the daemon.
	MaxChildren int
	// MaxDepth is the maximum delegation depth (depth-2 supersession). 0 → 2.
	MaxDepth int

	// DefaultVM is the VM a newly-dispatched worker is assigned to ("" = unassigned).
	// MaxWorkersPerVM caps concurrent (non-terminal) workers on a VM (0 = unlimited);
	// the cap is enforced only when a worker has a non-empty VM. Set from config.
	DefaultVM       string
	MaxWorkersPerVM int

	// VMs is the named-VM registry (rev7/T3.3), built by the daemon from the
	// configured fleet ([[vms]] → vm.NewRemote per host). nil = routing OFF:
	// today's single-client behavior, VM names stay pure labels. Non-nil =
	// routing ON: every op that acts on a specific worker resolves through
	// vmFor — its entry, or e.VM for "" — and a named VM with no entry is
	// ErrUnknownVM, never a silent fallback to the local client.
	VMs map[string]core.VMClient

	// RollupInterval coalesces supersession rollups: at most one rollup brain call
	// per session per interval when its children complete (0 = rollup disabled).
	RollupInterval time.Duration

	// ConfigDir is the daemon-owned base for per-worker worktrees + compiled
	// configs (OUTSIDE the worktree, B6). Required by Spawn; set by the daemon.
	ConfigDir string
	// GitBin is the git binary for worktree provisioning ("" → "git").
	GitBin string
	// InjectSkills names the built-in agent skill bundles to install into each
	// claude-kind worker's worktree (.claude/skills/) at spawn. Empty = none. Set
	// by the daemon (e.g. "arco-image" when the Telegram image relay is enabled).
	InjectSkills []string
	// DefaultPool is the provider pool a spawned worker draws a concurrency lease
	// from ("" = no lease). LeaseTTL is that lease's TTL. Both from config; the
	// lease is acquired atomically in Spawn's create tx (admission before intent).
	DefaultPool string
	LeaseTTL    time.Duration

	// Creds resolves a spawned worker's SCOPED provider env from its pool's clavis
	// profile (injected post-scrub at launch). nil → workers launch credential-less
	// (the Fake/headless path). Set by the daemon when use_local_vm.
	Creds core.AgentCredentials

	// Metrics is an OPTIONAL instrumentation seam (rev7/T2.5). It is an
	// interface, not a concrete type, so this package stays free of any
	// prometheus import — the daemon hands in *api.Metrics, which satisfies it.
	// nil (every existing test, and any embedder that doesn't care) = no-op:
	// every call goes through the nil-safe helpers below.
	Metrics Metrics

	// CI gates verification leg 1 (rev7/T3.1): the sweep polls a
	// completed_candidate worker's GitHub check-runs and records the outcome as
	// ledger evidence (verification_artifact / confirm escalation). CI success
	// is evidence for the human, never verification — Verify stays the only
	// path to completed_verified. Zero value = disabled.
	CI CICfg

	// Earn-out (rev7/T3.5): the sweep may resolve a pending DRAFTED question
	// with the brain's own draft once its question_class has proven itself.
	// VerificationLive is set by the daemon ONLY while a verification leg is
	// enabled (T3.1 ci_check_runs or T3.2 merge_queue) — earned-out autonomy
	// without machine verification downstream is never allowed. The Min* knobs
	// are the class's required human track record; a non-positive value
	// disables promotion outright (zero/unset must never mean "promote
	// instantly"). Confirms are never auto-answered, whatever the stats.
	VerificationLive    bool
	EarnOutMinDecisions int
	EarnOutMinAgreement float64

	// EStopPath is the operator emergency-stop sentinel (see Paused): a file
	// next to the ledger whose EXISTENCE pauses all new/autonomous work. Set by
	// the daemon; "" (tests, embedded use) = no estop surface.
	EStopPath string

	// AgentKind is the herdr agent kind Spawn launches (`agent start --kind`);
	// "" → "claude". herdr's supervision API (list/prompt/wait/kill, pane
	// liveness) is kind-agnostic, so any kind herdr knows can be supervised.
	// SAFETY: the compiled permission surface (permcompile settings/hooks) is
	// CLAUDE-ONLY — a non-claude kind launches with AgentArgs alone and NONE of
	// those guardrails. AgentArgs are extra argv appended to the agent
	// invocation (after the compiled claude args when kind is claude; the whole
	// argv otherwise).
	AgentKind string
	AgentArgs []string

	// IntakeMaster is the intake HMAC master secret (cfg.IntakeSecret, set by
	// the daemon). When non-empty, Spawn writes each worker's DERIVED key
	// (intakekey.Derive(master, workerID)) to <worker-root>/creds/intake_key
	// (T3.4) — the master itself never reaches a worker, in env, argv, or file.
	// "" = unsigned mode, no key file.
	IntakeMaster string

	// SelfOpWindow is how long after arco touched a pane (NoteSelfPaneOp) the
	// activity herdr pushes for that pane still counts as arco's own echo rather
	// than human presence. 0 → defaultSelfOpWindow.
	SelfOpWindow time.Duration
	// ActivityRestoreAfter is the quiet period after which Sweep returns a session
	// the human-activity back-off demoted to auto. 0 → defaultActivityRestoreAfter
	// (long by design — never an instant restore).
	ActivityRestoreAfter time.Duration

	// SpawnUID is the UID the daemon spawns workers under (its own UID, when the
	// local herdr launches them), recorded on the worker row at spawn so the UDS
	// intake can bind incoming events to it via SO_PEERCRED (a same-box HMAC
	// holder can't forge events for a worker recorded under another UID). nil =
	// don't record (Fake/cross-VM) → the intake leaves the worker ungated.
	SpawnUID *int

	mu     sync.Mutex
	misses map[string]int // workerID → consecutive missed sweeps (in-memory)
	// redriving guards against a sweep stacking duplicate crash-recovery re-drives
	// for one worker: brainClassify's brain_intent marker is committed ASYNC (on
	// Exec), so between a sweep's Submit and that commit the intent still looks
	// dangling, and a later sweep (under Exec backlog) would re-submit → two
	// classifications → a duplicate prompt/child (qwen review). At-most-one
	// in-flight re-drive per worker closes it; cleared on completion (in-memory —
	// after a crash the sweep correctly re-drives from the ledger).
	redriving map[string]bool
	// selfOps is paneID → when arco last touched that pane, and activityDemoted is
	// sessionID → when the human-activity back-off last saw activity for a session
	// IT demoted (D9, T3.6). Both are in-memory and written from the herdrsock read
	// goroutine while Sweep reads them on the timer goroutine — hence e.mu.
	selfOps         map[string]time.Time
	activityDemoted map[string]time.Time
}

// New builds an Engine with default thresholds and an Exec (brain disabled).
// MaxChildren defaults to a bounded fan-in (defense-in-depth: an Engine built
// outside the daemon still can't spawn unbounded children); the daemon overrides
// it from config. MaxDepth's 0→2 default is applied in Delegate.
func New(store core.Store, vm core.VMClient) *Engine {
	return &Engine{Store: store, VM: vm, Exec: NewExec(4), MissThreshold: 3,
		MaxChildren: 8, misses: map[string]int{}, redriving: map[string]bool{},
		selfOps: map[string]time.Time{}, activityDemoted: map[string]time.Time{}}
}

// DispatchResult reports what a dispatch created. State is the worker's state
// after the launch attempt (running, or failed if the launch failed) so callers
// can distinguish "launched" from "created-then-failed" without a follow-up list.
type DispatchResult struct {
	SessionID string
	WorkerID  string
	State     core.WorkerState
}

// Dispatch creates (or reuses) a session and spawns a worker for task, crash-safe:
// pre-generate the ULID + deterministic workspace, write dispatch_intent +
// CreateWorker(starting) BEFORE the external side effect, launch via the VM, then
// dispatch_done + transition to running. A crash between intent and done leaves a
// recoverable worker with no dispatch_done (boot recovery re-drives — later pass).
func (e *Engine) Dispatch(ctx context.Context, sessionRef, task string, newSession bool) (DispatchResult, error) {
	if e.Paused() {
		return DispatchResult{}, core.ErrPaused
	}
	// Resolve the assigned VM's client BEFORE any durable write or launch: with
	// routing on, an unresolvable VM refuses the dispatch outright (T3.3).
	vmc, err := e.vmFor(e.DefaultVM)
	if err != nil {
		return DispatchResult{}, err
	}
	workerID := ulid.Make().String()
	workspace := "arco_" + workerID
	var sessionID string

	// Phase 1: durable intent + worker row (before any external side effect).
	err = e.Store.WithTx(ctx, func(tx core.Tx) error {
		if newSession {
			sessionID = ulid.Make().String()
			if err := tx.CreateSession(core.Session{
				ID: sessionID, Goal: task, Status: core.SessionActive, Kind: core.SessionKindWork,
			}); err != nil {
				return err
			}
		} else {
			s, err := tx.ResolveSession(sessionRef)
			if err != nil {
				return err
			}
			if s.Kind == core.SessionKindPool {
				return core.ErrProtectedPool
			}
			sessionID = s.ID
		}
		// Per-VM concurrency admission (race-free in this create tx under the
		// single-writer lock) before the worker row exists.
		if err := e.admitVM(tx, e.DefaultVM); err != nil {
			return err
		}
		// Worker row first so the intent event's FK (events.worker_id) is satisfied;
		// both writes are in this one atomic tx, so crash-safety is unaffected.
		if err := tx.CreateWorker(core.Worker{
			ID: workerID, OwnerSession: sessionID, State: core.WorkerStarting,
			VM: e.DefaultVM, Workspace: workspace, Task: task, RunReason: "dispatch",
			IntakeUID: e.SpawnUID,
		}); err != nil {
			return err
		}
		_, _, _, err := tx.AppendEvent(core.Event{
			Kind: "dispatch_intent", SessionID: sessionID, WorkerID: workerID, Actor: "cli",
			Payload: fmt.Sprintf(`{"task":%q,"workspace":%q}`, task, workspace),
		})
		return err
	})
	if err != nil {
		return DispatchResult{}, err
	}

	// Phases 2+3: launch the agent + durable result/state.
	finalState, err := e.launchAndFinalize(ctx, vmc, workerID, workspace, sessionID, task)
	if err != nil {
		return DispatchResult{}, err
	}
	return DispatchResult{SessionID: sessionID, WorkerID: workerID, State: finalState}, nil
}

// admitVM enforces the per-VM concurrency cap inside a create tx (race-free
// under the single-writer lock). No-op when the worker has no VM assigned or the
// cap is unset — so it's inert until a VM-assigning deployment configures both.
// NB: the cap bounds workers THIS ledger tracks on the VM label, not the
// physical host — out-of-band launches on the same machine aren't counted
// (correct under the intended single-daemon/single-ledger deployment).
func (e *Engine) admitVM(tx core.Tx, vm string) error {
	if vm == "" || e.MaxWorkersPerVM <= 0 {
		return nil
	}
	n, err := tx.CountActiveWorkersOnVM(vm)
	if err != nil {
		return err
	}
	if n >= e.MaxWorkersPerVM {
		return core.ErrVMAtCapacity
	}
	return nil
}

// launchAndFinalize performs the external launch (phase 2) then the durable
// dispatch_done transition (phase 3) shared by Dispatch and Delegate, on the
// worker's assigned VM client (vmc, resolved by the caller pre-intent). A launch
// error is resolved by liveness (the agent may have spawned before the error
// surfaced) rather than blindly marking failed over a live process.
func (e *Engine) launchAndFinalize(ctx context.Context, vmc core.VMClient, workerID, workspace, sessionID, task string) (core.WorkerState, error) {
	launchErr := vmc.Prompt(ctx, workspace, task)
	finalState := core.WorkerRunning
	if launchErr != nil {
		finalState = core.WorkerFailed
		if agents, aerr := vmc.ListAgents(ctx); aerr == nil {
			for _, a := range agents {
				if a.Alive && a.Workspace == workspace {
					finalState = core.WorkerRunning
					break
				}
			}
		}
	}
	// Finalize on e.bg(), NOT the request ctx: after phase 1's durable intent, the
	// dispatch_done transition must land even if the caller disconnected, or the
	// worker is stranded `starting` forever (the launched agent unmanaged).
	err := e.Store.WithTx(e.bg(), func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		payload := "{}"
		if launchErr != nil {
			payload = fmt.Sprintf(`{"error":%q}`, launchErr.Error())
		}
		return tx.TransitionWorker(workerID, finalState, w.Rev, core.Event{
			Kind: "dispatch_done", WorkerID: workerID, SessionID: sessionID, Payload: payload,
		})
	})
	if err == nil && finalState == core.WorkerFailed {
		e.notifyCard(sessionID, notify.Card{
			Level: notify.LevelWarn,
			Title: "arco: worker failed — " + workerID,
			Body:  fmt.Sprintf("worker: %s\nlaunch failed: %v", workerID, launchErr),
		})
	}
	return finalState, err
}

// EventInput is a normalized worker state-change from the herdr hook / intake.
type EventInput struct {
	WorkerID     string
	HerdrState   string
	Alive        bool
	ObservedHead string
	WaitingInput bool
}

// ApplyEvent reconciles one worker against an observation: record the
// observation, fuse to a target state, and transition under a CAS if it changed.
// Deterministic (no brain call); ambiguity is left for a later brain pass.
func (e *Engine) ApplyEvent(ctx context.Context, in EventInput) error {
	var ambiguous bool
	var openedEsc bool    // a question escalation was NEWLY opened in the tx (no dedup)
	var task, sess string // the opened worker's task + session, for the decision card
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(in.WorkerID)
		if err != nil {
			return err
		}
		// observed_head arrives from (untrusted) intake and flows verbatim into
		// git/gh command lines downstream (mergeq fetch/merge, civerify's
		// `gh api .../commits/<head>/check-runs`). Gate it to a plausible commit
		// id HERE, at the boundary, so a worker cannot poison head_commit with an
		// `--upload-pack=…` / `../` / metacharacter payload; a bad shape is simply
		// dropped (the aliveness signal still lands, the head stays what it was).
		observedHead := in.ObservedHead
		if observedHead != "" && !core.LooksLikeRev(observedHead) {
			observedHead = ""
		}
		headChanged := observedHead != "" && observedHead != w.HeadCommit
		if err := tx.ObserveWorker(in.WorkerID, core.WorkerObservation{HeadCommit: observedHead}); err != nil {
			return err
		}
		target, amb := fusion.Resolve(w.State, fusion.Signals{
			HerdrState: in.HerdrState, Alive: in.Alive, HeadChanged: headChanged, WaitingInput: in.WaitingInput,
		})
		// A `blocked` worker is parked (e.g. by a brain billing wall) — do NOT
		// re-invoke the brain on the next ambiguous signal, or a parked worker
		// becomes a clavis/quota storm. It stays parked until an explicit reopen.
		// A POOL-OWNED worker (released via handoff, unowned pending claim) is
		// likewise inert to the brain — else it re-classifies in a paid loop,
		// escalates/delegates on the protected pool sentinel, and shares one
		// rate bucket with every pooled worker (opus review of the handoff wiring).
		ambiguous = amb && !w.State.Terminal() && w.State != core.WorkerBlocked &&
			w.OwnerSession != core.PoolSessionID
		if amb || target == w.State {
			return nil // ambiguity is handled off the write path, post-commit
		}
		if !core.LegalWorkerTransition(w.State, target) {
			return nil // keep current; illegal target means our signals lag reality
		}
		if err := tx.TransitionWorker(in.WorkerID, target, w.Rev, core.Event{
			Kind: "state_change", WorkerID: in.WorkerID, SessionID: w.OwnerSession,
			Payload: fmt.Sprintf(`{"herdr_state":%q,"target":%q}`, in.HerdrState, target),
		}); err != nil {
			if errors.Is(err, core.ErrRevMismatch) {
				return nil // another writer moved it first; the sweep will re-derive
			}
			return err
		}
		// A worker awaiting input surfaces a question escalation (shadow: the
		// operator decides; no unattended auto-answer in P2). One-pending-per-worker
		// is enforced by OpenEscalation, so repeat events don't pile up.
		if target == core.WorkerWaitingForUser {
			// Detect NEWLY-opened for the notify card: OpenEscalation dedupes to an
			// existing pending row silently, and a dedup must not re-notify. Reading
			// the pending state in the SAME serialized tx is race-free.
			pend, err := tx.ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: in.WorkerID})
			if err != nil {
				return err
			}
			if _, err := tx.OpenEscalation(core.Escalation{
				WorkerID: in.WorkerID, SessionID: w.OwnerSession, Kind: "question",
				QuestionClass: "clarify", ActionClass: core.ClassAmbiguous, Tier: core.TierMedium,
				Action: "worker is awaiting input",
			}); err != nil {
				return err
			}
			if len(pend) == 0 {
				openedEsc, task, sess = true, w.Task, w.OwnerSession
			}
			return nil
		}
		// If the worker LEFT a waiting state by another path (e.g. a later herdr
		// signal), close any lingering pending escalation so it isn't a phantom.
		if isWaiting(w.State) && !isWaiting(target) {
			if _, err := tx.ExpirePendingForWorker(in.WorkerID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// POST-COMMIT: push the decision card for a newly-opened escalation.
	if openedEsc {
		card := notify.EscalationCard{
			WorkerID: in.WorkerID,
			TaskTail: taskTail(task),
			Question: "worker is awaiting input",
		}
		if esc, ok := e.pendingEscForCard(in.WorkerID); ok {
			card.EscalationID, card.Kind, card.SessionID = esc.ID, esc.Kind, sess
			card.Draft, card.Confidence, card.Rationale = esc.DraftAnswer, esc.DraftConfidence, esc.BrainRationale
		}
		e.notifyCard(sess, notify.FormatEscalation(card))
	}
	// Ambiguous signals → ask the brain to classify, OFF the write path, serialized
	// per worker via Exec (Submit fires strictly after the commit above; no
	// reentrancy). Deterministic states never reach the brain.
	if ambiguous && e.Brain.Enabled && e.Exec != nil {
		wid := in.WorkerID
		e.Exec.Submit(wid, func() { e.brainClassify(e.bg(), wid) })
	}
	return nil
}

// bg returns the long-lived background context for off-write-path work.
func (e *Engine) bg() context.Context {
	if e.BgCtx != nil {
		return e.BgCtx
	}
	return context.Background()
}

func isWaiting(s core.WorkerState) bool {
	return s == core.WorkerWaitingForUser || s == core.WorkerWaitingConfirmation
}

// KillWorker terminates a worker on operator request: transition it to `killed`
// (if legal and not already terminal), expire any pending escalation, and STOP
// its herdr agent. The ledger change is durable; stopping the agent is a
// best-effort post-commit side effect (its worker is already killed regardless).
// This is the operator's way to terminate a runaway/wedged worker + reclaim its
// agent (capstone audit MED-3).
func (e *Engine) KillWorker(ctx context.Context, workerID string) error {
	var ref, vmName string
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		w, err := tx.GetWorker(workerID)
		if err != nil {
			return err
		}
		if w.State.Terminal() {
			return nil // already done — idempotent
		}
		if !core.LegalWorkerTransition(w.State, core.WorkerKilled) {
			return fmt.Errorf("%w: cannot kill from %s", core.ErrIllegalTransition, w.State)
		}
		ref, vmName = w.AgentRef, w.VM
		if _, err := tx.ExpirePendingForWorker(workerID); err != nil {
			return err
		}
		return tx.TransitionWorker(workerID, core.WorkerKilled, w.Rev, core.Event{
			Kind: "state_change", WorkerID: workerID, SessionID: w.OwnerSession, Actor: "operator",
			Payload: `{"reason":"operator_kill"}`,
		})
	})
	if err != nil {
		return err
	}
	if ref != "" {
		// Best-effort: stop the agent + reclaim its pane, ON THE WORKER'S VM —
		// pane ids are per-host, so another VM's client must never see this ref.
		// An unresolvable VM leaves nothing to stop with (the ledger kill stands;
		// the agent, if any, awaits the fleet being fixed + the orphan reaper).
		if vmc, verr := e.vmFor(vmName); verr == nil {
			_ = vmc.Kill(ctx, ref)
		}
	}
	return nil
}

// promptTarget is the VM target for prompting/killing a worker: its captured
// backend handle (herdr pane_id, in AgentRef) when arco launched it, else the
// workspace label. herdr's `agent prompt`/`send-keys` address a PANE, so a
// repo-spawned worker MUST be targeted by its pane_id — the workspace label only
// works for the Fake / legacy prompt-path (which never captures a pane_id).
// Live-verified on herdr 0.7.5 (the label is not a valid prompt target there).
func promptTarget(w core.Worker) string {
	if w.AgentRef != "" {
		return w.AgentRef
	}
	return w.Workspace
}
