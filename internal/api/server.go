// Package api serves the daemon's HTTP/JSON control surface over a unix socket
// (build-guide PASS-1). It is a thin inbound adapter: it decodes requests,
// calls the reconcile engine / ledger reader, and encodes JSON. No business
// logic lives here.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/intakekey"
	"github.com/dinhlongviolin1/arco/internal/mergeq"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
)

// maxIntakeBody caps the event-intake request body — a semi-trusted hook/herdr
// must not be able to OOM the daemon or bloat the ledger with one giant POST.
const maxIntakeBody = 1 << 20 // 1 MiB

// Server wires the ledger reader + reconcile engine into HTTP handlers.
type Server struct {
	store        core.Store
	eng          *reconcile.Engine
	mux          *http.ServeMux
	intakeSecret string // HMAC key for signed intake (P4); "" = unauthenticated (socket-only)
	mq           *mergeq.Queue
	imageRelay   ImageRelay // outbound image relay (telegram); nil = disabled → 503
}

// EnableMergeQueue installs the merge queue behind /v1/queue (rev7/T3.2).
// Unset (merge_queue = false), those routes answer 503.
func (s *Server) EnableMergeQueue(q *mergeq.Queue) { s.mq = q }

// SetIntakeSecret installs the shared secret for HMAC-signed event intake
// (security precondition P4). Set once at startup; when set, every POST
// /v1/events must carry a valid X-Arco-Signature.
func (s *Server) SetIntakeSecret(secret string) { s.intakeSecret = secret }

// New builds a Server and registers routes.
func New(store core.Store, eng *reconcile.Engine) *Server {
	s := &Server{store: store, eng: eng, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /v1/workers", s.listWorkers)
	s.mux.HandleFunc("GET /v1/sessions", s.listSessions)
	s.mux.HandleFunc("POST /v1/sessions/{id}/mode", s.setSessionMode)
	s.mux.HandleFunc("GET /v1/status", s.status)
	s.mux.HandleFunc("POST /v1/dispatch", s.dispatch)
	s.mux.HandleFunc("GET /v1/pools", s.listPools)
	s.mux.HandleFunc("POST /v1/pools", s.createPool)
	s.mux.HandleFunc("POST /v1/events", s.intake)
	s.mux.HandleFunc("GET /v1/workers/{id}/diff", s.workerDiff)
	s.mux.HandleFunc("POST /v1/workers/{id}/verify", s.verify)
	s.mux.HandleFunc("POST /v1/workers/{id}/kill", s.killWorker)
	s.mux.HandleFunc("POST /v1/workers/{id}/redeliver", s.redeliver)
	s.mux.HandleFunc("POST /v1/queue", s.queueEnqueue)
	s.mux.HandleFunc("GET /v1/queue", s.queueList)
	s.mux.HandleFunc("GET /v1/autonomy", s.autonomy)
	s.mux.HandleFunc("GET /v1/escalations", s.listEscalations)
	s.mux.HandleFunc("POST /v1/escalations/answer", s.answer)
	s.mux.HandleFunc("POST /v1/escalations/confirm", s.confirm)
	s.mux.HandleFunc("POST /v1/image/send", s.imageSend)
	return s
}

// Handler exposes the router (useful for httptest).
func (s *Server) Handler() http.Handler { return s.mux }

// Serve blocks serving HTTP on ln (typically a unix listener).
func (s *Server) Serve(ln net.Listener) error {
	return (&http.Server{Handler: s.mux}).Serve(ln)
}

// ---- wire DTOs (shared with the client) ------------------------------------

type HealthResp struct {
	Status string `json:"status"`
}

type DispatchReq struct {
	Task    string `json:"task"`
	Session string `json:"session"` // slug|id; empty with New=true creates one
	New     bool   `json:"new"`
	// Repo (optional): when set, dispatch takes the repo-based SPAWN path
	// (provision worktree → quarantine → compile config → launch), at Base
	// (commit-ish; "" = repo tip). Empty Repo keeps the prompt-based path.
	Repo string `json:"repo"`
	Base string `json:"base"`
	// Vm (optional, spawn path): the VM the worker runs on ("" = engine default).
	// Lets one session hold agents on different VMs.
	Vm string `json:"vm"`
}
type DispatchResp struct {
	SessionID string `json:"session_id"`
	WorkerID  string `json:"worker_id"`
	State     string `json:"state"` // worker state after dispatch (running, or failed if launch failed)
}

type WorkerDTO struct {
	ID    string `json:"id"`
	State string `json:"state"`
	VM    string `json:"vm"`
	Task  string `json:"task"`
	Owner string `json:"owner_session"`
	Rev   int64  `json:"rev"`
}
type WorkersResp struct {
	Workers []WorkerDTO `json:"workers"`
}

type SessionDTO struct {
	ID     string `json:"id"`
	Slug   string `json:"slug"`
	Status string `json:"status"`
	Goal   string `json:"goal"`
}
type SessionsResp struct {
	Sessions []SessionDTO `json:"sessions"`
}

// ModeReq sets a session's D9 supervision mode (auto|assist|manual).
type ModeReq struct {
	Mode string `json:"mode"`
}

// StatusResp is the one-call fleet snapshot (rev7/T1.2): workers by state,
// sessions by status, pending escalations with age, pool lease usage.
type StatusEscalationDTO struct {
	ID         string `json:"id"`
	Worker     string `json:"worker"`
	Kind       string `json:"kind"`
	Action     string `json:"action"`
	AgeSeconds int64  `json:"age_seconds"`
}
type PoolStatusDTO struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	ActiveLeases int    `json:"active_leases"`
	MaxActive    int    `json:"max_active"`
}
type StatusResp struct {
	Status             string                `json:"status"` // always "ok" when the daemon answers
	Paused             bool                  `json:"paused"` // operator estop engaged (arco pause)
	Sessions           map[string]int        `json:"sessions"`
	Workers            map[string]int        `json:"workers"`
	PendingEscalations []StatusEscalationDTO `json:"pending_escalations"`
	Pools              []PoolStatusDTO       `json:"pools"`
}

// EventReq is the herdr-hook intake payload.
type EventReq struct {
	Source        string `json:"source"`
	SourceEventID string `json:"source_event_id"`
	Hash          string `json:"source_event_hash"`
	WorkerRef     string `json:"worker_ref"` // worker id (workspace names are refused, T3.4)
	HerdrState    string `json:"herdr_state"`
	Alive         bool   `json:"alive"`
	ObservedHead  string `json:"observed_head"`
	WaitingInput  bool   `json:"waiting_input"`
	OccurredAt    string `json:"occurred_at"`
	// Audit tail: a non-empty DeniedCapability marks a worker's ATTEMPT at a
	// deny-listed action (reported by its PreToolUse hook) → auto-pause + danger
	// escalation, instead of the normal liveness reconcile.
	DeniedCapability string `json:"denied_capability"`
	DeniedDetail     string `json:"denied_detail"`
}
type EventResp struct {
	Deduped bool   `json:"deduped"`
	Note    string `json:"note,omitempty"`
}

type EscalationDTO struct {
	ID          string `json:"id"`
	Worker      string `json:"worker"`
	Session     string `json:"session"`
	Kind        string `json:"kind"`
	ActionClass string `json:"action_class"`
	Tier        string `json:"tier"`
	Capability  string `json:"capability"`
	Action      string `json:"action"`
	Status      string `json:"status"`
	Draft       string `json:"draft"`
	// The brain's draft metadata, so an operator can judge a draft without
	// opening the ledger (rev7/T1.4). Zero values when there is no draft.
	DraftConfidence float64 `json:"draft_confidence"`
	BrainRationale  string  `json:"brain_rationale"`
}
type EscalationsResp struct {
	Escalations []EscalationDTO `json:"escalations"`
}
type AnswerReq struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Scope string `json:"scope"` // once | session (aka always)
}
type ConfirmReq struct {
	ID    string `json:"id"`
	Yes   bool   `json:"yes"`
	Scope string `json:"scope"`
}
type DecisionResp struct {
	OK bool `json:"ok"`
}

type DiffResp struct {
	Rev        int64  `json:"rev"` // echo back to /verify so it CASes against the reviewed version
	State      string `json:"state"`
	Base       string `json:"base"`
	Head       string `json:"head"`
	Files      int    `json:"files"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Patch      string `json:"patch"`
	Truncated  bool   `json:"truncated"`
}

type VerifyReq struct {
	ExpectedRev int64  `json:"expected_rev"`
	Actor       string `json:"actor"`
}

// QueueReq enqueues a worker's head onto the merge queue (rev7/T3.2).
type QueueReq struct {
	Worker string `json:"worker"`
}
type QueueEnqueueResp struct {
	ID string `json:"id"`
}
type QueueItemDTO struct {
	ID     string `json:"id"`
	Worker string `json:"worker"`
	Repo   string `json:"repo"`
	Head   string `json:"head"`
	Status string `json:"status"`
}
type QueueResp struct {
	Items []QueueItemDTO `json:"items"`
}

// AutonomyClassDTO is one question_class row of the earn-out report (T3.5).
type AutonomyClassDTO struct {
	Class    string `json:"class"`
	Agree    int    `json:"agree"`
	Total    int    `json:"total"`
	Promotes bool   `json:"promotes"`
}
type AutonomyResp struct {
	VerificationLive bool               `json:"verification_live"`
	MinDecisions     int                `json:"min_decisions"`
	MinAgreement     float64            `json:"min_agreement"`
	Classes          []AutonomyClassDTO `json:"classes"`
}

// ---- handlers --------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResp{Status: "ok"})
}

func (s *Server) listWorkers(w http.ResponseWriter, _ *http.Request) {
	ws, err := s.store.Reader().ListWorkers(core.WorkerFilter{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := WorkersResp{}
	for _, x := range ws {
		out.Workers = append(out.Workers, WorkerDTO{
			ID: x.ID, State: string(x.State), VM: x.VM, Task: x.Task, Owner: x.OwnerSession, Rev: x.Rev,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// PoolReq creates a provider pool (operator config; `arco pool create`).
type PoolReq struct {
	ID              string `json:"id"`
	ClavisProfile   string `json:"clavis_profile"` // the scoped creds a leased worker launches with
	Provider        string `json:"provider"`
	MaxActive       int    `json:"max_active"`
	MaxStartsPerMin int    `json:"max_starts_per_min"`
}

// PoolDTO / PoolsResp are the pool read shape.
type PoolDTO struct {
	ID            string `json:"id"`
	ClavisProfile string `json:"clavis_profile"`
	Provider      string `json:"provider"`
	MaxActive     int    `json:"max_active"`
	State         string `json:"state"`
}
type PoolsResp struct {
	Pools []PoolDTO `json:"pools"`
}

func (s *Server) listPools(w http.ResponseWriter, _ *http.Request) {
	ps, err := s.store.Reader().ListPools()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := PoolsResp{}
	for _, p := range ps {
		out.Pools = append(out.Pools, PoolDTO{ID: p.ID, ClavisProfile: p.ClavisProfile, Provider: p.Provider, MaxActive: p.MaxActive, State: string(p.State)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createPool(w http.ResponseWriter, r *http.Request) {
	var req PoolReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.ID == "" || req.ClavisProfile == "" {
		writeErr(w, http.StatusBadRequest, errors.New("id and clavis_profile required"))
		return
	}
	if err := s.store.WithTx(r.Context(), func(tx core.Tx) error {
		return tx.CreatePool(core.ProviderPool{
			ID: req.ID, ClavisProfile: req.ClavisProfile, Provider: req.Provider,
			MaxActive: req.MaxActive, MaxStartsPerMin: req.MaxStartsPerMin,
		})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	p, _ := s.store.Reader().GetPool(req.ID)
	writeJSON(w, http.StatusCreated, PoolDTO{ID: p.ID, ClavisProfile: p.ClavisProfile, Provider: p.Provider, MaxActive: p.MaxActive, State: string(p.State)})
}

func (s *Server) listSessions(w http.ResponseWriter, _ *http.Request) {
	ss, err := s.store.Reader().ListSessions(core.SessionFilter{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := SessionsResp{}
	for _, x := range ss {
		out.Sessions = append(out.Sessions, SessionDTO{ID: x.ID, Slug: x.Slug, Status: string(x.Status), Goal: x.Goal})
	}
	writeJSON(w, http.StatusOK, out)
}

// setSessionMode flips a session's D9 supervision mode on operator request:
// 400 on an unknown mode (validated before any write), 404 on an unknown
// session. Accepts a session id or slug, like dispatch --session.
func (s *Server) setSessionMode(w http.ResponseWriter, r *http.Request) {
	var req ModeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	m, err := core.ParseSupervisionMode(req.Mode)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.WithTx(r.Context(), func(tx core.Tx) error {
		sess, err := tx.ResolveSession(r.PathValue("id"))
		if err != nil {
			return err
		}
		return tx.SetSessionMode(sess.ID, m, "operator")
	}); err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, DecisionResp{OK: true})
}

// status aggregates the one-call fleet snapshot from the reader: workers by
// state, sessions by status, pending escalations with age, pool lease usage.
func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	r := s.store.Reader()
	ws, err := r.ListWorkers(core.WorkerFilter{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ss, err := r.ListSessions(core.SessionFilter{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	es, err := r.ListEscalations(core.EscalationFilter{Status: "pending"})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ps, err := r.ListPools()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := StatusResp{
		Status:             "ok",
		Paused:             s.eng.Paused(),
		Sessions:           map[string]int{}, // never nil: zero state serializes as {}
		Workers:            map[string]int{},
		PendingEscalations: []StatusEscalationDTO{},
		Pools:              []PoolStatusDTO{},
	}
	for _, x := range ws {
		out.Workers[string(x.State)]++
	}
	for _, x := range ss {
		out.Sessions[string(x.Status)]++
	}
	now := time.Now()
	for _, e := range es {
		out.PendingEscalations = append(out.PendingEscalations, StatusEscalationDTO{
			ID: e.ID, Worker: e.WorkerID, Kind: e.Kind, Action: e.Action,
			AgeSeconds: ageSeconds(e.RequestedAt, now),
		})
	}
	for _, p := range ps {
		n, err := r.CountActiveLeases(p.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		out.Pools = append(out.Pools, PoolStatusDTO{ID: p.ID, State: string(p.State), ActiveLeases: n, MaxActive: p.MaxActive})
	}
	writeJSON(w, http.StatusOK, out)
}

// ageSeconds turns an RFC3339Nano timestamp into non-negative elapsed seconds
// (0 on parse error — an unreadable clock must not poison the snapshot).
func ageSeconds(requestedAt string, now time.Time) int64 {
	ts, err := time.Parse(time.RFC3339Nano, requestedAt)
	if err != nil {
		return 0
	}
	if age := int64(now.Sub(ts).Seconds()); age > 0 {
		return age
	}
	return 0
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	var req DispatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Task == "" {
		writeErr(w, http.StatusBadRequest, errors.New("task required"))
		return
	}
	newSession := req.New || req.Session == ""
	var res reconcile.DispatchResult
	var err error
	if req.Repo != "" {
		res, err = s.eng.Spawn(r.Context(), req.Session, req.Task, newSession, req.Repo, req.Base, req.Vm)
	} else {
		res, err = s.eng.Dispatch(r.Context(), req.Session, req.Task, newSession)
	}
	if err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, DispatchResp{SessionID: res.SessionID, WorkerID: res.WorkerID, State: string(res.State)})
}

// intake is the herdr-hook target: append the raw delivery (idempotent dedup on
// source_event_id), and if newly seen, reconcile the worker.
// verifyIntakeSig checks an "sha256=<hex>" HMAC of body under secret, in
// constant time. A missing/malformed header or wrong length fails closed.
func verifyIntakeSig(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	want, err := hex.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

func (s *Server) intake(w http.ResponseWriter, r *http.Request) {
	// Read the raw body under a size cap first — needed both to bound memory and
	// to HMAC-verify over the exact bytes.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIntakeBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	var req EventReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Resolve the worker by ID ONLY (T3.4): the old workspace-name fallback was
	// a guessable spoof path (arco_<id> convention) and is removed for intake.
	// Resolution comes BEFORE signature verification because the expected key is
	// the WORKER's derived key, not the master.
	var wk core.Worker
	found := false
	if req.WorkerRef != "" {
		w2, err := s.store.Reader().GetWorker(req.WorkerRef)
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		wk, found = w2, err == nil
	}
	if !found {
		ws, wsFound, err := s.workerByWorkspace(req.WorkerRef)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if wsFound {
			// A ref that is a workspace NAME (not a worker id) is refused in BOTH
			// signed and unsigned modes and never applied as a signal. Audited like
			// the peer-cred denial below (deduped on the delivery id); the 403 must
			// not depend on the ledger write succeeding.
			_ = s.store.WithTx(r.Context(), func(tx core.Tx) error {
				_, _, _, e := tx.AppendEvent(core.Event{
					Kind: "intake_denied", WorkerID: ws.ID, SessionID: ws.OwnerSession,
					Source: srcOrDefault(req.Source), SourceEventID: req.SourceEventID, SourceEventHash: req.Hash,
					Payload: fmt.Sprintf(`{"reason":"workspace_ref","ref":%q}`, req.WorkerRef),
				})
				return e
			})
			writeErr(w, http.StatusForbidden, errors.New("api: workspace-name worker_ref is not accepted; use the worker id"))
			return
		}
		if s.intakeSecret != "" {
			// No worker → no derivable key → the delivery cannot be authenticated.
			writeErr(w, http.StatusUnauthorized, errors.New("api: missing or invalid X-Arco-Signature"))
			return
		}
		// Unsigned mode, truly unknown ref: record a note (deduped on the delivery
		// id so herdr retries don't flood events) and ack 202 (never fail the hook).
		_ = s.store.WithTx(r.Context(), func(tx core.Tx) error {
			_, _, _, e := tx.AppendEvent(core.Event{Kind: "note", Source: srcOrDefault(req.Source),
				SourceEventID: req.SourceEventID, SourceEventHash: req.Hash,
				Payload: `{"note":"event for unknown worker_ref"}`})
			return e
		})
		writeJSON(w, http.StatusAccepted, EventResp{Note: "unknown worker_ref"})
		return
	}
	// Signed intake (P4, per-worker since T3.4): when a master secret is
	// configured, every event must carry a valid HMAC-SHA256 over the raw body
	// under THIS worker's derived key — the raw master and other workers' keys
	// never authenticate a delivery.
	if s.intakeSecret != "" && !verifyIntakeSig(intakekey.Derive(s.intakeSecret, wk.ID), body, r.Header.Get("X-Arco-Signature")) {
		writeErr(w, http.StatusUnauthorized, errors.New("api: missing or invalid X-Arco-Signature"))
		return
	}
	workerID := wk.ID

	// SO_PEERCRED gate (rev7/T1.6): a worker recorded under a spawn-time UID
	// only accepts events from that UID — an intake key alone can't forge
	// events for another UID's worker from the same box. A mismatch is audited
	// (deduped on the delivery id like every intake write) and answered 403; the
	// event is NEVER applied as a liveness/state signal. No peer UID on the ctx
	// (TCP/httptest) or no recorded UID → ungated, exactly as before.
	if wk.IntakeUID != nil {
		if peerUID, ok := peerUIDFrom(r.Context()); ok && peerUID != *wk.IntakeUID {
			// 403 even if the audit write fails: the denial decision must not
			// depend on the ledger being writable.
			_ = s.store.WithTx(r.Context(), func(tx core.Tx) error {
				_, _, _, e := tx.AppendEvent(core.Event{
					Kind: "intake_denied", WorkerID: workerID, SessionID: wk.OwnerSession,
					Source: srcOrDefault(req.Source), SourceEventID: req.SourceEventID, SourceEventHash: req.Hash,
					Payload: fmt.Sprintf(`{"peer_uid":%d,"expected_uid":%d}`, peerUID, *wk.IntakeUID),
				})
				return e
			})
			writeErr(w, http.StatusForbidden, errors.New("api: peer UID does not match the worker's recorded spawn UID"))
			return
		}
	}

	// Audit tail: a deny-listed-capability attempt is not a liveness signal — it
	// auto-pauses the worker + opens a danger escalation (idempotent on the
	// delivery id). Ack 200 so the hook never blocks.
	if req.DeniedCapability != "" {
		if err := s.eng.AuditDeniedAttempt(r.Context(), workerID, req.DeniedCapability, req.DeniedDetail, req.SourceEventID); err != nil {
			writeErr(w, errStatus(err), err)
			return
		}
		writeJSON(w, http.StatusOK, EventResp{Note: "audit: deny-listed attempt"})
		return
	}

	var deduped, conflict bool
	err = s.store.WithTx(r.Context(), func(tx core.Tx) error {
		_, d, c, e := tx.AppendEvent(core.Event{
			Source: srcOrDefault(req.Source), SourceEventID: req.SourceEventID, SourceEventHash: req.Hash,
			Kind: "state_change", WorkerID: workerID, OccurredAt: req.OccurredAt,
			Payload: `{"delivery":"herdr"}`,
		})
		deduped, conflict = d, c
		return e
	})
	if err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	if conflict {
		// same source_event_id, different hash → poisoned redelivery. The ledger
		// recorded an error event; do NOT reconcile off unverified content.
		writeJSON(w, http.StatusConflict, EventResp{Note: "source_event_hash conflict"})
		return
	}
	if deduped {
		writeJSON(w, http.StatusOK, EventResp{Deduped: true})
		return
	}
	if err := s.eng.ApplyEvent(r.Context(), reconcile.EventInput{
		WorkerID: workerID, HerdrState: req.HerdrState, Alive: req.Alive,
		ObservedHead: req.ObservedHead, WaitingInput: req.WaitingInput,
	}); err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, EventResp{Deduped: false})
}

func (s *Server) workerDiff(w http.ResponseWriter, r *http.Request) {
	wk, err := s.store.Reader().GetWorker(r.PathValue("id"))
	if err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	out := DiffResp{Rev: wk.Rev, State: string(wk.State), Base: wk.BaseCommit, Head: wk.HeadCommit}
	if wk.HeadCommit != "" { // avoid a git call (and a 500) on a worker with no head yet
		d, err := s.eng.WorkerDiff(r.Context(), wk.ID)
		if err != nil {
			writeErr(w, errStatus(err), err)
			return
		}
		out.Files, out.Insertions, out.Deletions, out.Patch, out.Truncated = d.Files, d.Insertions, d.Deletions, d.Patch, d.Truncated
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	var req VerifyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.eng.Verify(r.Context(), r.PathValue("id"), req.ExpectedRev, req.Actor); err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, DecisionResp{OK: true})
}

// killWorker terminates a worker + stops its agent (operator action; audit MED-3).
func (s *Server) killWorker(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.KillWorker(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, DecisionResp{OK: true})
}

// redeliver re-prompts a stranded running worker with its original task
// (operator recovery for a crash-lost initial delivery; audit MED-3).
func (s *Server) redeliver(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.RedeliverInitialTask(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, DecisionResp{OK: true})
}

// queueEnqueue puts a worker on the merge queue; processing itself happens on
// the daemon's sweep cadence, never inline in the request.
func (s *Server) queueEnqueue(w http.ResponseWriter, r *http.Request) {
	if s.mq == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("merge queue disabled (set merge_queue = true)"))
		return
	}
	var req QueueReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Worker == "" {
		writeErr(w, http.StatusBadRequest, errors.New("worker required"))
		return
	}
	id, err := s.mq.Enqueue(r.Context(), req.Worker)
	if err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, QueueEnqueueResp{ID: id})
}

func (s *Server) queueList(w http.ResponseWriter, r *http.Request) {
	if s.mq == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("merge queue disabled (set merge_queue = true)"))
		return
	}
	items, err := s.mq.Items(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := QueueResp{Items: []QueueItemDTO{}}
	for _, it := range items {
		out.Items = append(out.Items, QueueItemDTO{
			ID: it.ID, Worker: it.WorkerID, Repo: it.Repo, Head: it.Head, Status: it.Status,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// autonomy is the earn-out report (rev7/T3.5): per question_class, the human
// track record on drafted escalations and whether the class currently promotes
// under the live gates.
func (s *Server) autonomy(w http.ResponseWriter, _ *http.Request) {
	rep, err := s.eng.EarnOutReport()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := AutonomyResp{
		VerificationLive: s.eng.VerificationLive,
		MinDecisions:     s.eng.EarnOutMinDecisions,
		MinAgreement:     s.eng.EarnOutMinAgreement,
		Classes:          []AutonomyClassDTO{},
	}
	for _, c := range rep {
		out.Classes = append(out.Classes, AutonomyClassDTO{Class: c.Class, Agree: c.Agree, Total: c.Total, Promotes: c.Promotes})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listEscalations(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	if status == "all" {
		status = ""
	}
	es, err := s.store.Reader().ListEscalations(core.EscalationFilter{Status: status})
	if err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	out := EscalationsResp{}
	for _, e := range es {
		out.Escalations = append(out.Escalations, EscalationDTO{
			ID: e.ID, Worker: e.WorkerID, Session: e.SessionID, Kind: e.Kind,
			ActionClass: string(e.ActionClass), Tier: string(e.Tier), Capability: e.Capability,
			Action: e.Action, Status: e.Status, Draft: e.DraftAnswer,
			DraftConfidence: e.DraftConfidence, BrainRationale: e.BrainRationale,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var req AnswerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Via the Engine (not a bare tx) so a resume also DELIVERS the answer to the
	// worker's agent post-commit (audit MED-2) — the ledger resume alone leaves the
	// agent parked with the answer never typed in.
	if err := s.eng.AnswerQuestion(r.Context(), req.ID, req.Text, parseScope(req.Scope)); err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, DecisionResp{OK: true})
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	var req ConfirmReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Via the Engine so an approval also delivers to the worker's agent (MED-2).
	if err := s.eng.DecideConfirm(r.Context(), req.ID, req.Yes, parseScope(req.Scope)); err != nil {
		writeErr(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, DecisionResp{OK: true})
}

// parseScope maps the wire scope ("session"/"always" → session, else once).
func parseScope(s string) core.Scope {
	if s == "session" || s == "always" {
		return core.ScopeSession
	}
	return core.ScopeOnce
}

// workerByWorkspace reports the worker whose workspace label matches ref, if
// any. Used ONLY to refuse (403 + audit) intake refs that name a workspace —
// workspace names are guessable and no longer resolve a worker (T3.4).
func (s *Server) workerByWorkspace(ref string) (core.Worker, bool, error) {
	if ref == "" {
		return core.Worker{}, false, nil
	}
	ws, err := s.store.Reader().ListWorkers(core.WorkerFilter{})
	if err != nil {
		return core.Worker{}, false, err
	}
	for _, w := range ws {
		if w.Workspace == ref {
			return w, true, nil
		}
	}
	return core.Worker{}, false, nil
}

func srcOrDefault(s string) string {
	if s == "" {
		return "herdr"
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// errStatus maps domain sentinels to HTTP status codes.
func errStatus(err error) int {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, core.ErrProtectedPool):
		return http.StatusConflict
	case errors.Is(err, core.ErrIllegalTransition), errors.Is(err, core.ErrRevMismatch),
		errors.Is(err, core.ErrHighBlastScope), errors.Is(err, core.ErrEscalationState),
		errors.Is(err, core.ErrAgentBusy), errors.Is(err, core.ErrLeaseRejected):
		return http.StatusConflict // admission conflict (incl. pool-lease refusal) — not a server fault
	case errors.Is(err, core.ErrPaused), errors.Is(err, core.ErrVMAtCapacity):
		return http.StatusServiceUnavailable // estop / at-capacity — temporary, retriable
	default:
		return http.StatusInternalServerError
	}
}
