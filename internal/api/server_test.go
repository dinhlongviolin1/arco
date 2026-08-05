package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

func newTestAPI(t *testing.T) *httptest.Server {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "api.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	srv := New(s, reconcile.New(s, vm.NewFake()))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, ts *httptest.Server, path string, in any, out any) int {
	t.Helper()
	b, _ := json.Marshal(in)
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	if out != nil {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

func TestAPI_Health(t *testing.T) {
	ts := newTestAPI(t)
	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var h HealthResp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&h))
	require.Equal(t, "ok", h.Status)
}

func TestAPI_DispatchRequiresTask(t *testing.T) {
	ts := newTestAPI(t)
	code := post(t, ts, "/v1/dispatch", DispatchReq{Task: ""}, nil)
	require.Equal(t, http.StatusBadRequest, code)
}

func TestAPI_DispatchThenWorkers(t *testing.T) {
	ts := newTestAPI(t)
	var d DispatchResp
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d))
	require.NotEmpty(t, d.WorkerID)

	resp, err := http.Get(ts.URL + "/v1/workers")
	require.NoError(t, err)
	defer resp.Body.Close()
	var ws WorkersResp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&ws))
	require.Len(t, ws.Workers, 1)
	require.Equal(t, "running", ws.Workers[0].State)
}

func TestAPI_IntakeUnknownWorkerIsAccepted(t *testing.T) {
	ts := newTestAPI(t)
	var r EventResp
	code := post(t, ts, "/v1/events", EventReq{WorkerRef: "ghost", HerdrState: "idle"}, &r)
	require.Equal(t, http.StatusAccepted, code)
	require.Equal(t, "unknown worker_ref", r.Note)
}

func TestAPI_IntakeHashConflictIsRejectedNotApplied(t *testing.T) {
	ts := newTestAPI(t)
	var d DispatchResp
	post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d)
	require.Equal(t, "running", d.State)

	// first delivery: idle + HEAD advance → completed_candidate
	ev := EventReq{Source: "herdr:vm0", SourceEventID: "e1", Hash: "h1", WorkerRef: d.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: "abc"}
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/events", ev, &EventResp{}))

	// poisoned redelivery: SAME id, DIFFERENT hash + a state that would drive
	// the worker elsewhere. Must be rejected (409) and NOT applied.
	bad := ev
	bad.Hash = "h2"
	bad.HerdrState = "done"
	bad.Alive = false
	bad.ObservedHead = ""
	require.Equal(t, http.StatusConflict, post(t, ts, "/v1/events", bad, &EventResp{}))

	resp, err := http.Get(ts.URL + "/v1/workers")
	require.NoError(t, err)
	defer resp.Body.Close()
	var ws WorkersResp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&ws))
	require.Equal(t, "completed_candidate", ws.Workers[0].State, "poisoned redelivery must not re-drive the worker")
}

func TestAPI_DispatchReportsState(t *testing.T) {
	ts := newTestAPI(t)
	var d DispatchResp
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d))
	require.Equal(t, "running", d.State)
}

func TestAPI_VerifyDiffGate(t *testing.T) {
	ts := newTestAPI(t)
	var d DispatchResp
	post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d)

	// verify while running → 409
	require.Equal(t, http.StatusConflict, post(t, ts, "/v1/workers/"+d.WorkerID+"/verify", nil, &DecisionResp{}))

	// drive to completed_candidate
	post(t, ts, "/v1/events", EventReq{Source: "h", SourceEventID: "e1", Hash: "h1", WorkerRef: d.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: "abc"}, &EventResp{})

	// diff endpoint works
	resp, err := http.Get(ts.URL + "/v1/workers/" + d.WorkerID + "/diff")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// verify now succeeds → 200
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/workers/"+d.WorkerID+"/verify", nil, &DecisionResp{}))

	wr, err := http.Get(ts.URL + "/v1/workers")
	require.NoError(t, err)
	var ws WorkersResp
	require.NoError(t, json.NewDecoder(wr.Body).Decode(&ws))
	wr.Body.Close()
	require.Equal(t, "completed_verified", ws.Workers[0].State)
}

func TestAPI_EscalationFlow(t *testing.T) {
	ts := newTestAPI(t)
	var d DispatchResp
	post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d)

	// worker awaits input → opens a question escalation
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/events",
		EventReq{Source: "herdr:v", SourceEventID: "e1", Hash: "h1", WorkerRef: d.WorkerID, HerdrState: "idle", Alive: true, WaitingInput: true}, &EventResp{}))

	resp, err := http.Get(ts.URL + "/v1/escalations")
	require.NoError(t, err)
	var es EscalationsResp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&es))
	resp.Body.Close()
	require.Len(t, es.Escalations, 1)
	require.Equal(t, "question", es.Escalations[0].Kind)

	// answer it → 200, worker resumes running
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/escalations/answer",
		AnswerReq{ID: es.Escalations[0].ID, Text: "proceed", Scope: "once"}, &DecisionResp{}))

	wr, err := http.Get(ts.URL + "/v1/workers")
	require.NoError(t, err)
	var ws WorkersResp
	json.NewDecoder(wr.Body).Decode(&ws)
	wr.Body.Close()
	require.Equal(t, "running", ws.Workers[0].State)
}

func TestAPI_IntakeDedup(t *testing.T) {
	ts := newTestAPI(t)
	var d DispatchResp
	post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d)

	ev := EventReq{Source: "herdr:vm0", SourceEventID: "e1", Hash: "h1", WorkerRef: d.WorkerID, HerdrState: "idle", Alive: true, ObservedHead: "abc"}
	var r1, r2 EventResp
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/events", ev, &r1))
	require.False(t, r1.Deduped)
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/events", ev, &r2))
	require.True(t, r2.Deduped)
}
