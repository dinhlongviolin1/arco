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
	"io"
	"net"
	"net/http"

	"github.com/dinhlongviolin1/arco/internal/core"
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
}

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
	s.mux.HandleFunc("POST /v1/dispatch", s.dispatch)
	s.mux.HandleFunc("POST /v1/events", s.intake)
	s.mux.HandleFunc("GET /v1/workers/{id}/diff", s.workerDiff)
	s.mux.HandleFunc("POST /v1/workers/{id}/verify", s.verify)
	s.mux.HandleFunc("GET /v1/escalations", s.listEscalations)
	s.mux.HandleFunc("POST /v1/escalations/answer", s.answer)
	s.mux.HandleFunc("POST /v1/escalations/confirm", s.confirm)
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

// EventReq is the herdr-hook intake payload.
type EventReq struct {
	Source        string `json:"source"`
	SourceEventID string `json:"source_event_id"`
	Hash          string `json:"source_event_hash"`
	WorkerRef     string `json:"worker_ref"` // worker id or workspace
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
		res, err = s.eng.Spawn(r.Context(), req.Session, req.Task, newSession, req.Repo, req.Base)
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
	// Signed intake (P4): when a shared secret is configured, every event must
	// carry a valid HMAC-SHA256 over the raw body — this source-binds cross-VM /
	// network intake so an unauthenticated poster can't inject worker events.
	if s.intakeSecret != "" && !verifyIntakeSig(s.intakeSecret, body, r.Header.Get("X-Arco-Signature")) {
		writeErr(w, http.StatusUnauthorized, errors.New("api: missing or invalid X-Arco-Signature"))
		return
	}
	var req EventReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	workerID, ok, err := s.resolveWorker(req.WorkerRef)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		// Unknown worker: record a note (deduped on the delivery id so herdr
		// retries don't flood events) and ack 202 (never fail the hook).
		_ = s.store.WithTx(r.Context(), func(tx core.Tx) error {
			_, _, _, e := tx.AppendEvent(core.Event{Kind: "note", Source: srcOrDefault(req.Source),
				SourceEventID: req.SourceEventID, SourceEventHash: req.Hash,
				Payload: `{"note":"event for unknown worker_ref"}`})
			return e
		})
		writeJSON(w, http.StatusAccepted, EventResp{Note: "unknown worker_ref"})
		return
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
	err := s.store.WithTx(r.Context(), func(tx core.Tx) error {
		return tx.AnswerQuestion(req.ID, req.Text, parseScope(req.Scope), core.Event{
			Kind: "question_esc", Payload: `{"decided_by":"human"}`,
		})
	})
	if err != nil {
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
	err := s.store.WithTx(r.Context(), func(tx core.Tx) error {
		return tx.DecideConfirm(req.ID, req.Yes, parseScope(req.Scope), core.Event{
			Kind: "confirm_dec", Payload: `{"decided_by":"human"}`,
		})
	})
	if err != nil {
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

// resolveWorker accepts a worker id or a workspace name.
func (s *Server) resolveWorker(ref string) (string, bool, error) {
	if ref == "" {
		return "", false, nil
	}
	if w, err := s.store.Reader().GetWorker(ref); err == nil {
		return w.ID, true, nil
	} else if !errors.Is(err, core.ErrNotFound) {
		return "", false, err
	}
	ws, err := s.store.Reader().ListWorkers(core.WorkerFilter{})
	if err != nil {
		return "", false, err
	}
	for _, w := range ws {
		if w.Workspace == ref {
			return w.ID, true, nil
		}
	}
	return "", false, nil
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
		errors.Is(err, core.ErrHighBlastScope), errors.Is(err, core.ErrEscalationState):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
