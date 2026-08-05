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
type Reader interface {
	GetWorker(id string) (Worker, error)
	ListWorkers(f WorkerFilter) ([]Worker, error)
	GetSession(id string) (Session, error)
	ResolveSession(ref string) (Session, error) // id | slug
	ListSessions(f SessionFilter) ([]Session, error)
	EventsSince(cursor int64, limit int) ([]Event, error)
	ListEscalations(f EscalationFilter) ([]Escalation, error)
	GetEscalation(id string) (Escalation, error)
	Capability(name string) (CatalogRow, bool, error)
	DefaultTree() ([]CatalogRow, error)
	// Allowed is the authoritative capability check for arco-executed actions
	// (O(1) own-tree read; cascade keeps it O(1)).
	Allowed(sessionID, capability string) (bool, error)

	// GetPool returns a provider pool by id.
	GetPool(id string) (ProviderPool, error)
	// CountActiveLeases returns how many un-released leases the pool holds.
	CountActiveLeases(poolID string) (int, error)
}

// Tx is a serialized single-writer transaction. It is arco-owned; the storage
// engine (*sql.Tx) never leaks through it. CAS mutators take an expectedRev
// and return ErrRevMismatch. All reads inside a Tx see uncommitted writes.
type Tx interface {
	Reader

	CreateWorker(w Worker) error
	TransitionWorker(id string, to WorkerState, expectedRev int64, e Event) error
	AppendEvent(e Event) (cursor int64, deduped bool, conflict bool, err error)

	// ObserveWorker records liveness/HEAD observations (head_commit, last_seen_at,
	// pid, boot_id) without a state change or rev bump — the sweep/intake truth.
	ObserveWorker(id string, obs WorkerObservation) error

	CreateSession(s Session) error
	SetSessionStatus(id string, to SessionStatus, expectedRev int64, e Event) error
	AttachWorker(sessionID, workerID string) error

	Grant(sessionID, capability, grantedBy string, e Event) (newPermRev int64, err error)
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
	// ExpirePendingForWorker closes any pending escalation for a worker that has
	// left its waiting state by another path (so it doesn't linger as a phantom).
	ExpirePendingForWorker(workerID string) (int, error)

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
	Close() error
}

// ---- worker-execution port (LocalVMClient now; SSHVMClient in P3) ----------

// AgentObs is one observed agent from a VM. Identity is
// vm+workspace+boot_id+pid_start_time — never a central PID.
type AgentObs struct {
	Workspace    string
	BootID       string
	PIDStartTime string
	Alive        bool
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
	Kill(ctx context.Context, workspace string) error
	Diff(ctx context.Context, worktree, base, head string) (Diff, error)
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
