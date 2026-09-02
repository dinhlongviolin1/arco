package core

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors returned across the port boundary.
var (
	// ErrRevMismatch is returned by a CAS mutator when the caller's expectedRev
	// no longer matches the row — "no side effect without a successful CAS".
	ErrRevMismatch = errors.New("core: optimistic rev mismatch")
	// ErrNotFound is returned when a looked-up entity does not exist.
	ErrNotFound = errors.New("core: not found")
	// ErrIllegalTransition is returned for a disallowed worker state change.
	ErrIllegalTransition = errors.New("core: illegal worker state transition")
	// ErrProtectedPool guards mutations that are illegal on the pool sentinel.
	ErrProtectedPool = errors.New("core: operation not allowed on the protected pool session")
	// ErrLeaseRejected is returned by AcquireLease when pool admission denies the
	// lease. Inspect the wrapped *LeaseRejection for the machine-readable Reason.
	ErrLeaseRejected = errors.New("core: lease admission rejected")
	// ErrNotPooled is returned when claiming a worker that is not currently in the
	// pool (only released/pooled workers can be claimed).
	ErrNotPooled = errors.New("core: worker is not pooled")
	// ErrMaxDepthExceeded is returned when a delegation would exceed the maximum
	// delegation depth (depth-2 supersession).
	ErrMaxDepthExceeded = errors.New("core: delegation depth limit exceeded")
	// ErrFanInExceeded is returned when a session already holds the maximum number
	// of active workers (max_children_per_session fan-in cap).
	ErrFanInExceeded = errors.New("core: session fan-in limit exceeded")
	// ErrVMAtCapacity is returned when a VM already runs the maximum number of
	// active workers (per-VM concurrency admission).
	ErrVMAtCapacity = errors.New("core: VM at worker capacity")
	// ErrUnknownVM is returned when VM routing is enabled (a fleet registry is
	// configured) and a worker's VM name has no client in it — the op is refused
	// rather than silently falling back to the local client.
	ErrUnknownVM = errors.New("core: unknown VM")
	// ErrPaused is returned when the operator's emergency stop (the ESTOP
	// sentinel) is engaged: no new workers, no autonomous actions. In-flight
	// work is never killed; operator-initiated actions still run.
	ErrPaused = errors.New("core: arco is paused — run `arco resume` (or remove the ESTOP file)")
	// ErrAgentBusy is returned when an action would interrupt an agent that is
	// observably working/blocked (e.g. redelivering its task).
	ErrAgentBusy = errors.New("core: agent is busy")
)

// LeaseRejection carries WHY a lease was denied (disabled|cooldown|at_capacity|
// start_rate) so a caller can back off appropriately. It satisfies
// errors.Is(err, ErrLeaseRejected).
type LeaseRejection struct{ Reason string }

func (e *LeaseRejection) Error() string        { return "core: lease rejected: " + e.Reason }
func (e *LeaseRejection) Is(target error) bool { return target == ErrLeaseRejected }

// StartRateWindow is the sliding window over which MaxStartsPerMin is enforced.
const StartRateWindow = time.Minute

// WorkerObservation carries liveness/HEAD facts recorded by the sweep or intake.
// Empty string fields are left unchanged; LastSeenAt defaults to now if empty.
type WorkerObservation struct {
	HeadCommit   string
	PID          *int
	PIDStartTime string
	BootID       string
	LastSeenAt   string
}

// WorkerFilter selects workers for a list query (zero value = all).
type WorkerFilter struct {
	State        WorkerState
	VM           string
	OwnerSession string
}

// SessionFilter selects sessions for a list query (zero value = all).
type SessionFilter struct {
	Status SessionStatus
	Kind   SessionKind
}

// Reader is the read side of the ledger (WAL concurrent reads). It never
// exposes the storage engine.
// Message is one durable conversation turn — the append-only, per-session chat
// history the brain can query on demand (ADR 0003). Content is Scrub()'d at
// AppendMessage (the write chokepoint), so no secret persists; Tainted marks
// brain-sourced (untrusted) content. Stored in brain_transcript_rows and mirrored
// into the transcript_fts FTS5 index.
type Message struct {
	ID        int64
	SessionID string
	Role      string // "operator" | "arco"
	Content   string
	Tainted   bool
	CreatedAt time.Time
}

type Reader interface {
	GetWorker(id string) (Worker, error)
	ListWorkers(f WorkerFilter) ([]Worker, error)
	GetSession(id string) (Session, error)
	ResolveSession(ref string) (Session, error) // id | slug
	ListSessions(f SessionFilter) ([]Session, error)
	EventsSince(cursor int64, limit int) ([]Event, error)
	// RecentWorkerEvents returns up to limit of a worker's most recent events in
	// chronological (id-ascending) order — the tail used to assemble brain context.
	RecentWorkerEvents(workerID string, limit int) ([]Event, error)
	// StaleBrainIntents returns the IDs of running/starting, non-pool workers whose
	// most-recent brain_intent was recorded before `before` and has no event
	// sharing its correlation_id — a classification lost to a crash before it acted.
	// The sweep re-drives these; a fired side effect leaves a cid-sibling, so the
	// re-drive can never duplicate a prompt or a delegated child.
	StaleBrainIntents(before time.Time) ([]string, error)
	ListEscalations(f EscalationFilter) ([]Escalation, error)
	GetEscalation(id string) (Escalation, error)
	// DraftAgreement is the per-question_class earn-out tally (rev7/T3.5): of
	// the HUMAN decisions taken on escalations that carried a brain DraftAnswer,
	// how many agreed with the draft. Ledger-backed — survives restart. An
	// untouched (or empty) class reads (0, 0, nil), never an error.
	DraftAgreement(questionClass string) (agree, total int, err error)
	Capability(name string) (CatalogRow, bool, error)
	DefaultTree() ([]CatalogRow, error)
	// Catalog returns the FULL capability_catalog (all rows incl. high-blast) —
	// permcompile.Compile's required input.
	Catalog() ([]CatalogRow, error)
	// Allowed is the authoritative capability check for arco-executed actions
	// (O(1) own-tree read; cascade keeps it O(1)).
	Allowed(sessionID, capability string) (bool, error)
	// GrantedCapabilities returns a session's effective granted capability set
	// (default-allowed ∪ active non-expired grants) — the `granted` input for
	// permcompile.Compile / LaunchArgs.
	GrantedCapabilities(sessionID string) (map[string]bool, error)
	// GrantedCapabilitiesForWorker is GrantedCapabilities scoped to one worker:
	// default-allowed ∪ session-wide (worker_id NULL) ∪ this worker's own grants,
	// excluding sibling workers' per-worker grants (issue-model isolation).
	GrantedCapabilitiesForWorker(sessionID, workerID string) (map[string]bool, error)

	// GetPool returns a provider pool by id.
	GetPool(id string) (ProviderPool, error)
	// ListPools returns all provider pools (operator view / `arco pool list`).
	ListPools() ([]ProviderPool, error)
	// CountActiveLeases returns how many un-released leases the pool holds.
	CountActiveLeases(poolID string) (int, error)
	// CountActiveWorkers returns how many NON-terminal workers a session owns
	// (the fan-in cap denominator for delegation admission).
	CountActiveWorkers(sessionID string) (int, error)
	// CountActiveWorkersOnVM returns how many NON-terminal workers are assigned to
	// a VM (the per-VM concurrency-admission denominator).
	CountActiveWorkersOnVM(vm string) (int, error)

	// RecentMessages returns a session's durable conversation messages at or after
	// `since`, bounded to the newest `limit`, in chronological (oldest-first) order
	// — the restart-surviving chat-history tail fed into the brain context. Only
	// active (non-compacted) rows. A hard server-side cap bounds limit so no caller
	// can request an unbounded window.
	RecentMessages(sessionID string, since time.Time, limit int) ([]Message, error)
	// SearchMessages returns a session's messages whose content matches the FTS5
	// query, newest-first, capped at limit — the brain's on-demand history fetch.
	SearchMessages(sessionID, query string, limit int) ([]Message, error)

	// DueScheduledTasks returns ENABLED scheduled tasks whose next_run is at or
	// before now, most-overdue first, capped at limit — the scheduler's hot query.
	DueScheduledTasks(now time.Time, limit int) ([]ScheduledTask, error)
	// ListScheduledTasks returns every scheduled task (for /schedule list).
	ListScheduledTasks() ([]ScheduledTask, error)
	// GetScheduledTask returns one task by id (ErrNotFound if absent).
	GetScheduledTask(id string) (ScheduledTask, error)
}

// ScheduledTask is a recurring/planned UNATTENDED agent run: when due, arco runs
// the chat brain (Converse) with the full tool surface + the task's own durable
// memory, in its own session/topic, so a monitoring task can inspect and
// (confirm-gated) act on the fleet.
type ScheduledTask struct {
	ID         string
	Name       string    // human label + topic name
	Schedule   string    // canonical spec: cron "0 8 * * *" or interval "30m"
	Prompt     string    // the agentic instruction the run receives
	SessionID  string    // the task's own session (its topic + durable memory)
	Enabled    bool
	NextRun    time.Time // when it next fires
	LastRun    time.Time // last fire (zero = never)
	LastStatus string    // "" | ok | error
	LastResult string    // last run's short outcome
	CreatedAt  time.Time
}

// Tx is a serialized single-writer transaction. It is arco-owned; the storage
// engine (*sql.Tx) never leaks through it. CAS mutators take an expectedRev
// and return ErrRevMismatch. All reads inside a Tx see uncommitted writes.
type Tx interface {
	Reader

	CreateWorker(w Worker) error
	TransitionWorker(id string, to WorkerState, expectedRev int64, e Event) error
	AppendEvent(e Event) (cursor int64, deduped bool, conflict bool, err error)

	// AppendMessage records one durable conversation turn (append-only, per
	// session). Content is Scrub()'d here — the write chokepoint, exactly like
	// event payloads — so a secret in chat never persists, and the row is mirrored
	// into transcript_fts for search. Returns the new message id.
	AppendMessage(m Message) (id int64, err error)

	// CreateScheduledTask inserts a new recurring/planned task.
	CreateScheduledTask(t ScheduledTask) error
	// RecordScheduledRun stamps a completed run: last_run, the newly-computed
	// next_run, the status ("ok"/"error") and a short result line.
	RecordScheduledRun(id string, lastRun, nextRun time.Time, status, result string) error
	// SetScheduledTaskEnabled pauses/resumes a task.
	SetScheduledTaskEnabled(id string, enabled bool) error
	// DeleteScheduledTask removes a task.
	DeleteScheduledTask(id string) error

	// ObserveWorker records liveness/HEAD observations (head_commit, last_seen_at,
	// pid, boot_id) without a state change or rev bump — the sweep/intake truth.
	ObserveWorker(id string, obs WorkerObservation) error

	// BumpWorkerStall increments workers.stall_count (the sweep's alive-no-progress
	// counter behind stall_n) and returns the new count. Observation-like: no state
	// change, no rev bump.
	BumpWorkerStall(id string) (int, error)
	// ResetWorkerStall zeroes workers.stall_count (HEAD advanced, or a stall was
	// resolved). No state change, no rev bump.
	ResetWorkerStall(id string) error

	// BindAgentRef records the VM-backend agent handle (e.g. herdr pane_id)
	// captured at launch, so the sweep can correlate this worker's liveness by it.
	BindAgentRef(workerID, ref string) error
	// BindLaunch records a worker's provisioned worktree, checked-out base commit,
	// backend agent handle (ref), and the agent's stable identity (bootID =
	// herdr terminal_id) at dispatch_done (the repo-based spawn path). Capturing
	// bootID here — not just later on the liveness alive-path — arms the identity
	// guard from birth, so a recycled pane_id can never be mistaken for this
	// worker's agent (relied on by the sweep's destructive orphan reaper). A "" ref
	// or "" bootID (launch error / unresolvable) is stored as-is (correlate by
	// workspace; the reaper declines an unidentifiable agent).
	BindLaunch(workerID, worktree, base, ref, bootID string) error

	CreateSession(s Session) error
	// CreatePool inserts a provider pool (operator config; `arco pool create`).
	CreatePool(p ProviderPool) error
	SetSessionStatus(id string, to SessionStatus, expectedRev int64, e Event) error
	// SetSessionMode sets a session's D9 supervision mode: validates the mode
	// (reject unknown BEFORE writing), bumps the session rev, and appends a
	// `mode_change` event attributed to actor with the old+new mode.
	SetSessionMode(id string, m SupervisionMode, actor string) error
	// SetSessionTelegram binds a session's Telegram forum topic id and/or pinned
	// status-card message id — arco-internal routing state (like BindAgentRef:
	// no rev bump, no event). A nil pointer leaves that column unchanged.
	SetSessionTelegram(id string, topicID, statusMsgID *int64) error
	AttachWorker(sessionID, workerID string) error

	Grant(sessionID, capability, grantedBy string, e Event) (newPermRev int64, err error)
	// GrantWorker is Grant scoped to a single worker (issue-model isolation);
	// workerID "" is identical to a session-wide Grant.
	GrantWorker(sessionID, workerID, capability, grantedBy string, e Event) (newPermRev int64, err error)
	Revoke(sessionID, capability string, e Event) (newPermRev int64, err error)

	// OpenEscalation opens a question/confirm. One pending escalation per worker:
	// a second open for the same worker+capability returns the existing id.
	OpenEscalation(esc Escalation) (id string, err error)
	// AnswerQuestion resolves a pending question with a HUMAN answer and resumes
	// the worker. scope=ScopeSession promotes a non-high-blast capability to a
	// standing grant; a high-blast capability with ScopeSession is rejected
	// (standing high-blast grants require an explicit CLI grant, never here).
	AnswerQuestion(id, text string, scope Scope, e Event) error
	// DecideConfirm resolves a pending danger-class confirm (yes/no). Same
	// scope/grant rules as AnswerQuestion.
	DecideConfirm(id string, yes bool, scope Scope, e Event) error
	// AnswerQuestionBrain resolves a pending drafted QUESTION with the brain's
	// own draft (rev7/T3.5 earn-out promotion), stamping answered_by/decided_by
	// 'brain'. AnswerQuestion/DecideConfirm stay the human path: this variant
	// takes no scope and can NEVER promote a grant (a brain-sourced approval/
	// grant must not exist) and never feeds the DraftAgreement tally (it would
	// ratify itself). Confirms are out of reach by kind. e must carry the audit
	// payload naming the class stats that justified the promotion.
	AnswerQuestionBrain(id, text string, e Event) error
	// ExpirePendingForWorker closes any pending escalation for a worker that has
	// left its waiting state by another path (so it doesn't linger as a phantom).
	ExpirePendingForWorker(workerID string) (int, error)
	// ExpireEscalation expires ONE escalation by id, only if still pending
	// (returns the number expired: 1 or 0). Unlike ExpirePendingForWorker this
	// targets the exact row the caller sampled, so the escalation-timeout reaper
	// can't expire a DIFFERENT (freshly-opened) escalation for the same worker
	// that was minted between its pending snapshot and this tx.
	ExpireEscalation(id string) (int, error)

	// AcquireLease atomically admits a new lease against poolID under the
	// single-writer lock (counts active + start-window leases, applies admission),
	// inserting an UNBOUND lease (worker_id NULL) with expires_at = now + ttl.
	// Returns *LeaseRejection (errors.Is ErrLeaseRejected) when admission denies,
	// or ErrNotFound if the pool doesn't exist. An expired cooldown auto-clears.
	AcquireLease(leaseID, poolID string, ttl time.Duration) error
	// BindLease attaches an acquired lease to the worker + dispatch_intent event
	// it admitted (called in the same tx as CreateWorker/dispatch_intent).
	BindLease(leaseID, workerID string, dispatchIntentEventID int64) error
	// ReleaseLease marks a lease released (idempotent: already-released is a no-op).
	ReleaseLease(leaseID string) error
	// SetPoolState sets a pool's admission state; cooldownUntil applies only to
	// PoolCooldown (RFC3339Nano; "" clears it). Used for 429 backoff / disable.
	SetPoolState(poolID string, state PoolState, cooldownUntil string) error
	// ReapLeases releases leaked/stale leases (build-guide B10-lease): unbound
	// leases past TTL, plus bound leases whose worker is terminal or gone. Returns
	// the number released.
	ReapLeases() (int, error)

	// ReleaseWorker hands a live worker back to the protected pool (owner →
	// sentinel, pooled_at = now): emits worker_release_intent + worker_released and
	// invalidates the compiled config (recompile-on-reassign). No-op if already
	// pooled; ErrIllegalTransition on a terminal worker.
	ReleaseWorker(workerID, actor string) error
	// ClaimWorker moves a POOLED worker to toSession (owner → toSession, pooled_at
	// cleared): emits worker_claim_intent + worker_claimed. ErrNotPooled if the
	// worker isn't in the pool; ErrProtectedPool if toSession is the pool.
	ClaimWorker(workerID, toSession, actor string) error
	// TransferWorker moves a worker directly between (non-pool) sessions: emits
	// worker_transfer_intent + worker_transferred. ErrProtectedPool if source or
	// target is the pool (release/claim handle the pool); no-op if already there.
	TransferWorker(workerID, toSession, actor string) error
	// ReapPooledWorkers pauses workers pooled longer than ttl (pool-TTL reaper):
	// an unclaimed worker shouldn't run forever. Returns the number paused.
	ReapPooledWorkers(ttl time.Duration) (int, error)

	// CountRecentBrainCalls counts brain_intent events for sessionID within the
	// last window — per-session brain-rate admission. Called inside the write tx
	// that records the next brain_intent so the count→admit→insert is race-free.
	CountRecentBrainCalls(sessionID string, window time.Duration) (int, error)
	// CountRecentRollups counts rollup_intent events for one PARENT WORKER within
	// the last window — the per-parent coalescing denominator for supersession
	// rollup (≤1 rollup per parent per interval).
	CountRecentRollups(parentWorkerID string, window time.Duration) (int, error)
}

// ErrHighBlastScope is returned when a caller tries to promote a high-blast
// capability to a standing grant via an escalation decision.
var ErrHighBlastScope = errors.New("core: high-blast capability cannot be granted via an escalation (use arco grant)")

// ErrEscalationState is returned when an escalation is decided in the wrong kind/status.
var ErrEscalationState = errors.New("core: escalation not pending or wrong kind")

// Store is the persistence port (P3 may swap SQLite for Postgres without
// touching callers). WithTx is synchronous; any reconcile-enqueue / broadcast
// fires strictly AFTER commit — no reentrancy into the store from within fn.
type Store interface {
	Migrate(ctx context.Context) error
	WithTx(ctx context.Context, fn func(Tx) error) error
	Reader() Reader
	// Now returns the store's current time from its injected clock — the SAME
	// source that stamps event recorded_at, so app-layer time cutoffs (the sweep's
	// stale-brain-intent grace) stay consistent with the ledger and controllable by
	// tests via SetClock.
	Now() time.Time
	Close() error
}

// ---- worker-execution port (LocalVMClient now; SSHVMClient in P3) ----------

// LaunchSpec is the pinned worker-launch request (security precond P6): the
// agent Kind, the daemon-owned Workdir (worktree), the pinned tool/permission
// Args (permcompile.LaunchArgs), and the scrubbed Env (spawnenv.Scrub). Name is
// arco's workspace id ("arco_<ulid>"), a stable label for the backend agent.
type LaunchSpec struct {
	Name    string
	Kind    string
	Workdir string
	Args    []string
	Env     []string
}

// AgentObs is one observed agent from a VM. Ref is the backend's own agent
// handle (herdr's pane_id) — the correlation key when a worker was launched via
// the arco-owned spawn path; Workspace is the fallback for Prompt-model/Fake
// workers. BootID is a stable per-agent identity for the PID-reuse guard (herdr
// exposes no PID, so its terminal_id fills this slot).
type AgentObs struct {
	Ref          string
	Workspace    string
	BootID       string
	PIDStartTime string
	State        string // backend agent status (herdr: idle|working|blocked|done|unknown)
	Alive        bool
	// Descriptive fields for operator-facing SCAN/adopt (herdr agent list carries
	// them); the sweep ignores these — it correlates purely on Ref/Workspace/BootID.
	Kind      string // agent kind, e.g. "claude"
	Cwd       string // the agent's working directory
	Title     string // terminal title (the agent's current task line)
	SessionID string // backend's own agent-session id (herdr agent_session.value) — the CLI resume handle
}

// ScannedAgent is one live agent observed on a VM (its AgentObs) annotated with
// the arco VM name and whether arco already tracks it. This is the SHARED shape
// for /scan and /adopt across every layer — the engine that produces it, the
// features that render it, and the telegram surface that displays it — so there
// is one type on the contract leaf, not a mirror per package. Adopt turns an
// untracked one into a monitored worker.
type ScannedAgent struct {
	AgentObs
	VM       string // arco VM name ("" = the local box)
	Tracked  bool   // already an arco worker
	WorkerID string // the tracking worker, when Tracked
}

// Diff is a base→head git diff summary.
type Diff struct {
	Base, Head string
	Files      int
	Insertions int
	Deletions  int
	Patch      string // full patch for a single worker; empty in bulk numstat
	Truncated  bool   // Patch was capped (too large to buffer whole)
}

// VMClient observes and drives workers on a machine.
type VMClient interface {
	ListAgents(ctx context.Context) ([]AgentObs, error)
	GitHeads(ctx context.Context, worktrees []string) (map[string]string, error)
	Prompt(ctx context.Context, workspace, text string) error // text embeds a prompt_intent ULID
	// PromptReady delivers a prompt to a JUST-LAUNCHED agent, CONFIRMING it landed
	// (the agent reacted) and retrying while the agent's TUI is still booting — the
	// plain Prompt races a fresh agent's startup and can silently no-op. Used for
	// the initial task delivery on the spawn path.
	PromptReady(ctx context.Context, workspace, text string) error
	// AgentStatus returns the backend's current status for a target (e.g. herdr's
	// idle|working|blocked|done|unknown), or "" when it can't be determined —
	// used to guard against re-prompting a busy agent. Never fatal: callers treat
	// "" as unknown and decide themselves.
	AgentStatus(ctx context.Context, target string) (string, error)
	Kill(ctx context.Context, workspace string) error
	// ReadPane returns the recent terminal output of an agent's pane (herdr `pane
	// read`), up to lines rows — a READ-ONLY peek used to summarize what a session
	// is doing. Best-effort: "" + error when the backend can't read it.
	ReadPane(ctx context.Context, ref string, lines int) (string, error)
	// Launch starts a NEW agent per spec and returns the backend's agent handle
	// (herdr pane_id, the ref for sweep correlation) AND its stable identity
	// (bootID = herdr terminal_id) captured at launch, so the identity guard is
	// armed from birth. The pinned launch flags + scrubbed env are in the spec.
	// bootID is "" if the backend exposes no stable id or it can't be resolved
	// right after start (the worker then falls back to identity-on-first-observe).
	Launch(ctx context.Context, spec LaunchSpec) (ref, bootID string, err error)
	Diff(ctx context.Context, worktree, base, head string) (Diff, error)
}

// AgentCredentials supplies the SCOPED provider env a spawned worker launches
// with — the env of its pool's clavis profile (ANTHROPIC_AUTH_TOKEN/BASE_URL +
// model vars). It is appended to the launch env AFTER the P1 scrub: this
// deliberately re-adds provider vars the scrub removed, but a pool-SCOPED set
// (a named clavis profile), NEVER arco's own inherited credentials — the
// provider-pool security model. An empty profile yields no vars (no injection),
// so a worker with no pool/profile launches credential-less (inert by default).
type AgentCredentials interface {
	EnvFor(ctx context.Context, profile string) ([]string, error) // "KEY=VALUE" lines; empty profile → nil
}

// ---- redaction port (write-time; deterministic + versioned) ----------------

// Scrubber removes secrets before persistence and before the brain prompt.
type Scrubber interface {
	Scrub(s string) (clean string, n int)
	Version() string
}

// ---- structural classifier ------------------------------------------------

// CapabilityOf maps an observed action to its catalog row. classified=false
// means unclassifiable ⇒ FAIL-CLOSED (treat as ask/deny, never routine).
type CapabilityOf func(e NormalizedEntry) (row CatalogRow, classified bool)

// ---- brain / normalize DTOs (implemented by later packages) ----------------

// NormalizedEntry is one entry of the per-agent normalized transcript stream.
type NormalizedEntry struct {
	Kind string // assistant|tool_call|tool_result|prompt_wait|billing|rate_limited|error
	Text string
	Tool string
	At   time.Time
}

// StepResult is the brain's typed decision. Every Kind has a reconciler branch;
// an unhandled kind becomes an error event, never a silent drop.
type StepResult struct {
	Kind        string // run_again|dispatch|handoff|final_output|question|confirm
	Worker      string
	Instruction string
	Reason      string
}

// DraftAnswer is what the two-level resolver returns. It has NO scope/yes-no/
// grant field: a brain-sourced value can never reach DecideConfirm/Grant.
type DraftAnswer struct {
	Text       string
	Confidence float64
	Rationale  string
	Tainted    bool // always true for brain output
}

// Clock is the injectable time source (never call time.Now directly in
// assembly paths, to keep context byte-stable).
type Clock func() time.Time
