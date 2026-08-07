package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// Guideline tests (rev7/T1.6): the UDS intake binds worker events to the
// worker's spawn-time UID via SO_PEERCRED. A holder of the HMAC secret on the
// same box must not be able to forge events for a worker spawned under a
// different UID. Non-cred transports (TCP) keep today's behavior. Do not
// weaken these asserts.

// newUDSAPI serves the API over a REAL unix socket with peer-cred capture
// wired (the same ConnContext the daemon installs), returning a client that
// dials it plus the store and engine for seeding.
func newUDSAPI(t *testing.T) (*http.Client, *ledger.Store, *reconcile.Engine) {
	t.Helper()
	dir := t.TempDir()
	s, err := ledger.Open(filepath.Join(dir, "api.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	eng := reconcile.New(s, vm.NewFake())
	srv := New(s, eng)

	sock := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	hs := &http.Server{Handler: srv.Handler(), ConnContext: PeerCredConnContext}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })

	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	return hc, s, eng
}

func postJSON(t *testing.T, hc *http.Client, path string, in any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(in)
	resp, err := hc.Post("http://unix"+path, "application/json", strings.NewReader(string(b)))
	require.NoError(t, err)
	defer resp.Body.Close()
	var buf [4096]byte
	n, _ := resp.Body.Read(buf[:])
	return resp.StatusCode, buf[:n]
}

func seedWorkerWithUID(t *testing.T, s *ledger.Store, uid *int) string {
	t.Helper()
	ctx := context.Background()
	session, worker := ulid.Make().String(), ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: session, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateWorker(core.Worker{ID: worker, OwnerSession: session, State: core.WorkerStarting,
			Workspace: "arco_" + worker, IntakeUID: uid})
	}))
	return worker
}

// Same UID (ours, over a real UDS) → event accepted exactly as before.
func TestIntake_PeerCred_SameUIDAccepted(t *testing.T) {
	hc, s, _ := newUDSAPI(t)
	me := os.Getuid()
	id := seedWorkerWithUID(t, s, &me)
	code, _ := postJSON(t, hc, "/v1/events", EventReq{
		WorkerRef: id, SourceEventID: "e1", HerdrState: "working", Alive: true,
	})
	require.Equal(t, http.StatusOK, code)
}

// A worker recorded under a DIFFERENT UID → 403 and an audit event on the
// worker, deduped-safe; the event must not be applied as a liveness signal.
func TestIntake_PeerCred_MismatchForbiddenAndAudited(t *testing.T) {
	hc, s, _ := newUDSAPI(t)
	other := os.Getuid() + 12345
	id := seedWorkerWithUID(t, s, &other)
	code, body := postJSON(t, hc, "/v1/events", EventReq{
		WorkerRef: id, SourceEventID: "e2", HerdrState: "working", Alive: true,
	})
	require.Equal(t, http.StatusForbidden, code, "got body: %s", body)

	evs, err := s.Reader().RecentWorkerEvents(id, 10)
	require.NoError(t, err)
	found := false
	for _, e := range evs {
		if e.Kind == "intake_denied" {
			found = true
		}
	}
	require.True(t, found, "a peer-cred mismatch must leave an intake_denied audit event")

	w, err := s.Reader().GetWorker(id)
	require.NoError(t, err)
	require.Equal(t, core.WorkerStarting, w.State, "forged event must not drive worker state")
}

// A worker with NO recorded UID (legacy rows, cross-VM) is not gated.
func TestIntake_PeerCred_NoRecordedUIDSkipsCheck(t *testing.T) {
	hc, s, _ := newUDSAPI(t)
	id := seedWorkerWithUID(t, s, nil)
	code, _ := postJSON(t, hc, "/v1/events", EventReq{
		WorkerRef: id, SourceEventID: "e3", HerdrState: "working", Alive: true,
	})
	require.Equal(t, http.StatusOK, code)
}

// Non-cred transport (plain TCP httptest, no ConnContext) keeps today's
// behavior even for a worker with a mismatched recorded UID: those paths are
// gated by HMAC + preflight, not peer creds.
func TestIntake_NonCredTransportUnchanged(t *testing.T) {
	s, err := ledger.Open(filepath.Join(t.TempDir(), "api.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(New(s, reconcile.New(s, vm.NewFake())).Handler())
	t.Cleanup(ts.Close)

	other := os.Getuid() + 12345
	id := seedWorkerWithUID(t, s, &other)
	code := post(t, ts, "/v1/events", EventReq{
		WorkerRef: id, SourceEventID: "e4", HerdrState: "working", Alive: true,
	}, nil)
	require.Equal(t, http.StatusOK, code)
}

// Dispatch through an engine with SpawnUID set must record the UID on the row
// (the "map recorded at spawn" leg).
func TestDispatch_RecordsSpawnUID(t *testing.T) {
	hc, s, eng := newUDSAPI(t)
	me := os.Getuid()
	eng.SpawnUID = &me

	code, body := postJSON(t, hc, "/v1/dispatch", DispatchReq{Task: "x", New: true})
	require.Equal(t, http.StatusOK, code)
	var d DispatchResp
	require.NoError(t, json.Unmarshal(body, &d))

	w, err := s.Reader().GetWorker(d.WorkerID)
	require.NoError(t, err)
	require.NotNil(t, w.IntakeUID)
	require.Equal(t, me, *w.IntakeUID)
}
