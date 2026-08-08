package api

// GUIDELINE TESTS — rev7 T3.4: per-worker HKDF intake keys on the wire; the
// workspace-name fallback in resolveWorker is REMOVED for intake.
//
// Pinned wire contract (only when an intake secret is configured; the
// unsigned local-socket mode is unchanged):
//   - POST /v1/events must be HMAC-signed with the WORKER's derived key
//     intakekey.Derive(master, workerID) — not the master.
//   - the raw master on the wire → 401 (it must never authenticate a delivery).
//   - another worker's key → 401 (keys are worker-bound).
//   - worker_ref that is a workspace name (not a worker id) → 403, event NOT
//     applied (the guessable-workspace spoof path is closed).
//   - rotating the master invalidates all previously derived keys.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/intakekey"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// keyedAPI is signedAPI + access to the store and server (for rotation).
func keyedAPI(t *testing.T, secret string) (*httptest.Server, *Server, *ledger.Store) {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "hkdf.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	srv := New(s, reconcile.New(s, vm.NewFake()))
	srv.SetIntakeSecret(secret)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv, s
}

func hmacHex(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestAPI_IntakeRequiresPerWorkerKey(t *testing.T) {
	ts, _, _ := keyedAPI(t, "master-secret")
	var d DispatchResp
	require.Equal(t, 200, post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d))

	body, _ := json.Marshal(EventReq{WorkerRef: d.WorkerID, HerdrState: "idle", Alive: true})
	workerKey := intakekey.Derive("master-secret", d.WorkerID)

	require.Equal(t, 401, rawPost(t, ts, "/v1/events", body, ""), "unsigned rejected")
	require.Equal(t, 401, rawPost(t, ts, "/v1/events", body, hmacHex("master-secret", body)),
		"the MASTER must never authenticate a delivery — only derived worker keys ride the wire")
	require.Equal(t, 401, rawPost(t, ts, "/v1/events", body, hmacHex(intakekey.Derive("master-secret", "some-other-worker"), body)),
		"another worker's key must not authenticate this worker's events")
	require.Equal(t, 200, rawPost(t, ts, "/v1/events", body, hmacHex(workerKey, body)),
		"the worker's own derived key authenticates")
}

func TestAPI_WorkspaceFallbackRemoved(t *testing.T) {
	ts, _, s := keyedAPI(t, "master-secret")
	var d DispatchResp
	require.Equal(t, 200, post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d))
	_, err := s.DB().Exec(`UPDATE workers SET workspace=? WHERE id=?`, "pane-alpha", d.WorkerID)
	require.NoError(t, err)

	// Even a correctly worker-key-signed body must not resolve via workspace name.
	body, _ := json.Marshal(EventReq{WorkerRef: "pane-alpha", HerdrState: "idle", Alive: true})
	sig := hmacHex(intakekey.Derive("master-secret", d.WorkerID), body)
	require.Equal(t, 403, rawPost(t, ts, "/v1/events", body, sig),
		"workspace-name refs are a guessable spoof path — removed, 403")

	// The same delivery by worker ID works.
	body, _ = json.Marshal(EventReq{WorkerRef: d.WorkerID, HerdrState: "idle", Alive: true})
	require.Equal(t, 200, rawPost(t, ts, "/v1/events", body,
		hmacHex(intakekey.Derive("master-secret", d.WorkerID), body)))

	// And the fallback stays removed in unsigned local mode too.
	ts2, _, s2 := keyedAPI(t, "")
	var d2 DispatchResp
	require.Equal(t, 200, post(t, ts2, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d2))
	_, err = s2.DB().Exec(`UPDATE workers SET workspace=? WHERE id=?`, "pane-beta", d2.WorkerID)
	require.NoError(t, err)
	body, _ = json.Marshal(EventReq{WorkerRef: "pane-beta", HerdrState: "idle", Alive: true})
	require.Equal(t, 403, rawPost(t, ts2, "/v1/events", body, ""))
	w, err := s2.Reader().GetWorker(d2.WorkerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w.State, "denied delivery is not applied as a signal")
}

func TestAPI_MasterRotationInvalidatesDerivedKeys(t *testing.T) {
	ts, srv, _ := keyedAPI(t, "gen1")
	var d DispatchResp
	require.Equal(t, 200, post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d))
	body, _ := json.Marshal(EventReq{WorkerRef: d.WorkerID, HerdrState: "idle", Alive: true})

	oldSig := hmacHex(intakekey.Derive("gen1", d.WorkerID), body)
	require.Equal(t, 200, rawPost(t, ts, "/v1/events", body, oldSig))

	srv.SetIntakeSecret("gen2")
	require.Equal(t, 401, rawPost(t, ts, "/v1/events", body, oldSig),
		"rotating the master must invalidate every previously derived worker key")
	require.Equal(t, 200, rawPost(t, ts, "/v1/events", body,
		hmacHex(intakekey.Derive("gen2", d.WorkerID), body)))
}
