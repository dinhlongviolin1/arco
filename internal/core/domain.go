// Package core is arco's dependency-free domain: entity types, enums, and the
// port interfaces (Store, VMClient, Scrubber, CapabilityOf) that adapters
// implement. It imports nothing arco-specific and no storage engine, so every
// other package can speak the domain without pulling in SQLite.
//
// Structure note (build-guide-rev6 §C): timestamps are stored as TEXT
// RFC3339Nano and represented at the Go seam as `string`, never time.Time, so
// byte-stable context assembly and engine-agnostic storage both hold.
package core

// WorkerState is the lifecycle state of a worker. `lost` = a worker whose
// process+workspace the VMClient cannot find across LivenessMissThreshold
// sweeps (distinct from failed/killed).
type WorkerState string

const (
	WorkerStarting            WorkerState = "starting"
	WorkerRunning             WorkerState = "running"
	WorkerWaitingForUser      WorkerState = "waiting_for_user"
	WorkerWaitingConfirmation WorkerState = "waiting_for_confirmation"
	WorkerBlocked             WorkerState = "blocked"
	WorkerCompletedCandidate  WorkerState = "completed_candidate"
	WorkerCompletedVerified   WorkerState = "completed_verified"
	WorkerFailed              WorkerState = "failed"
	WorkerPaused              WorkerState = "paused"
	WorkerKilled              WorkerState = "killed"
	WorkerLost                WorkerState = "lost"
)

// AllWorkerStates is the frozen enum set (must match the schema CHECK).
var AllWorkerStates = []WorkerState{
	WorkerStarting, WorkerRunning, WorkerWaitingForUser, WorkerWaitingConfirmation,
	WorkerBlocked, WorkerCompletedCandidate, WorkerCompletedVerified, WorkerFailed,
	WorkerPaused, WorkerKilled, WorkerLost,
}

// Terminal reports whether a state is terminal (no further transitions except
// via explicit reopen/recovery).
func (s WorkerState) Terminal() bool {
	switch s {
	case WorkerCompletedVerified, WorkerFailed, WorkerKilled, WorkerLost:
		return true
	}
	return false
}

// SessionStatus is the lifecycle state of a session.
type SessionStatus string

const (
	SessionOpen     SessionStatus = "open"
	SessionActive   SessionStatus = "active"
	SessionWaiting  SessionStatus = "waiting"
	SessionIdle     SessionStatus = "idle"
	SessionDone     SessionStatus = "done"
	SessionArchived SessionStatus = "archived"
)

// SessionKind distinguishes ordinary work sessions from the protected pool
// sentinel. PoolSessionID is the well-known 26-char Crockford ULID seeded by
// 0001_init.sql before any worker can be created.
type SessionKind string

const (
	SessionKindWork SessionKind = "work"
	SessionKindPool SessionKind = "pool"
)

// PoolSessionID is the fixed well-known ULID of the protected pool session.
const PoolSessionID = "00000000000000000000000000"

// ActionClass and Tier classify a capability (assigned structurally from the
// capability_catalog; NL/brain may only RAISE severity, never lower it).
type ActionClass string

const (
	ClassRoutine   ActionClass = "routine"
	ClassAmbiguous ActionClass = "ambiguous"
	ClassDanger    ActionClass = "danger"
)

type Tier string

const (
	TierLow       Tier = "low"
	TierMedium    Tier = "medium"
	TierHighBlast Tier = "high_blast"
)

// Scope is the grant scope on a human answer/decision. "session" on a
// non-high-blast capability promotes to a standing session_grants row; "once"
// does not.
type Scope string

const (
	ScopeOnce    Scope = "once"
	ScopeSession Scope = "session"
)

// Worker is a supervised coding-agent instance. Field list frozen (build-guide
// §C P2-3) so adapters don't diverge on names/types.
type Worker struct {
	ID              string
	Title           string
	VM              string
	Workspace       string // deterministic: arco_<ulid>
	Worktree        string
	BaseCommit      string
	HeadCommit      string
	Program         string
	AgentKind       string // claude|qwen|... selects the normalizer
	BootID          string
	PID             *int
	PIDStartTime    string
	State           WorkerState
	Rev             int64 // optimistic CAS
	StallCount      int
	OwnerSession    string // NOT NULL; pool = the sentinel
	SessionPermRev  int64
	PermissionsHash string
	CompiledConfig  string // path OUTSIDE the worktree
	Task            string
	RunReason       string
	ParentWorkerID  string
	DelegationDepth int
	Role            string
	Summary         string
	LastSeenAt      string
	LastEventAt     string
	PooledAt        string
	CreatedAt       string
	// AgentRef is the VM-backend's own agent handle captured at launch (herdr's
	// pane_id), the key the sweep correlates liveness by. "" = not launched via
	// the arco-owned spawn path → sweep falls back to a Workspace match.
	AgentRef string
	// IntakeUID is the UID the worker was spawned under (recorded by the daemon
	// on the local VM path). The UDS intake resolves the connecting peer's UID
	// via SO_PEERCRED and rejects events whose peer UID differs, so a same-box
	// holder of the intake HMAC secret can't forge per-worker events. nil =
	// unknown/ungated (legacy rows, cross-VM, Fake) → intake keeps today's
	// behavior.
	IntakeUID *int
}

// Session is the first-class unit of work + conversation; sessions form a
// single-parent tree (ParentSession, "" = root).
type Session struct {
	ID             string
	Slug           string
	Title          string
	Goal           string
	Status         SessionStatus
	Kind           SessionKind
	ParentSession  string
	Rev            int64
	PermRev        int64
	MemRev         int64
	Permissions    string // DERIVED cache of the grant rows
	ContextSummary string
	ContextRev     int64
	Facts          string
	Progress       string
	Repo           string
	DefaultVM      string
	Pinned         bool
	NotifyLevel    string
	TGTopicID      *int64
	TGStatusMsgID  *int64
	StallCount     int
	LastActivityAt string
	CreatedAt      string
	ClosedAt       string
}

// Event is one row of the immutable, append-only, two-clock event log.
type Event struct {
	ID               int64
	Source           string // internal|herdr:<vm>|cli|tg|web
	SourceEventID    string // stable id from origin; "" for internal
	SourceEventHash  string
	WorkerID         string
	SessionID        string
	Kind             string // see EventKind enum; schema column has NO CHECK (extensible)
	Actor            string
	CausationEventID *int64
	CorrelationID    string
	Payload          string // already Scrub()'d at write time
	OccurredAt       string
	RecordedAt       string
}

// Escalation is a stuck-worker question or a danger-class confirm awaiting a
// decision. In P2 autonomy is shadow/draft: the brain may fill DraftAnswer +
// BrainRationale, but only a human decision (DecidedBy="human") resolves it —
// a DraftAnswer never becomes the decision or reaches a grant.
type Escalation struct {
	ID                string
	WorkerID          string
	SessionID         string
	Kind              string // question | confirm
	QuestionClass     string
	ActionClass       ActionClass
	Tier              Tier
	Capability        string // "" for free-text questions
	ActionFingerprint string
	Action            string
	Detail            string
	DraftAnswer       string // shadow: the brain's advisory draft (never the decision)
	DraftConfidence   float64
	BrainRationale    string
	AnsweredBy        string // "" | brain | human
	Status            string // pending|answered|approved|rejected|expired
	Decision          string
	AnswerText        string
	DecidedBy         string
	OnceOrAlways      string // once | always
	RequestedAt       string
	DecidedAt         string
	ResumedAt         string
}

// EscalationFilter selects escalations (zero value = all).
type EscalationFilter struct {
	Status    string
	SessionID string
	WorkerID  string
}

// PoolState is a provider pool's admission state. `cooldown` is a temporary
// 429-backoff (auto-clears at CooldownUntil); `disabled` is operator-set and
// sticky. Pools cap rate-limit / concurrency ONLY — no cost (build-guide §A #1).
type PoolState string

const (
	PoolOK       PoolState = "ok"
	PoolCooldown PoolState = "cooldown"
	PoolDisabled PoolState = "disabled"
)

// ProviderPool caps how many workers may run against one provider/org/profile
// concurrently (MaxActive), how fast they may start (MaxStartsPerMin), and
// enforces a 429 cooldown. Mirrors the provider_pools row (cost fields cut).
type ProviderPool struct {
	ID              string
	Provider        string
	Org             string
	ClavisProfile   string
	ModelClass      string
	MaxActive       int
	MaxStartsPerMin int
	State           PoolState
	CooldownUntil   string // RFC3339Nano; "" when not in cooldown
	CreatedAt       string
}

// Lease is one worker's admission token against a pool, acquired BEFORE
// dispatch_intent and released when the worker terminates. A leaked lease (crash
// between acquire and release) is reaped by ReapLeases via TTL / terminal-worker
// cross-check (build-guide B10-lease). Mirrors the worker_pool_leases row.
type Lease struct {
	ID                    string
	PoolID                string
	WorkerID              string // "" until bound to a worker
	DispatchIntentEventID *int64
	AcquiredAt            string
	ExpiresAt             string
	ReleasedAt            string // "" while active
}

// CatalogRow is a capability_catalog row — the structural classifier's data.
type CatalogRow struct {
	Capability     string
	ActionClass    ActionClass
	Tier           Tier
	DefaultAllowed bool
	HighBlast      bool // never compiled onto a worker
	CompiledWorker bool // 0 = arco-executed-only
	Description    string
}
