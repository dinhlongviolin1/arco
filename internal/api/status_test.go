package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// Guideline tests (rev7/T1.2): GET /v1/status is the one-call fleet snapshot —
// sessions by status, workers by state, pending escalations with age, pool
// lease usage, and an explicit ok marker. Do not weaken these asserts.

func newStatusAPI(t *testing.T) (*httptest.Server, *ledger.Store) {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "api.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(New(s, reconcile.New(s, vm.NewFake())).Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func getStatus(t *testing.T, ts *httptest.Server) StatusResp {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/status")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out StatusResp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// Zero state must be a clean 200 snapshot, not a 500 or nil-map panic.
func TestStatus_ZeroState(t *testing.T) {
	ts, _ := newStatusAPI(t)
	got := getStatus(t, ts)
	require.Equal(t, "ok", got.Status)
	require.Empty(t, got.PendingEscalations)
	require.Empty(t, got.Pools)
	require.Zero(t, got.Workers["running"])
}

func TestStatus_SeededFleet(t *testing.T) {
	ts, s := newStatusAPI(t)
	ctx := context.Background()

	session := ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: session, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))

	// one starting + one running worker
	starting, running := ulid.Make().String(), ulid.Make().String()
	for _, id := range []string{starting, running} {
		id := id
		require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
			return tx.CreateWorker(core.Worker{ID: id, OwnerSession: session, State: core.WorkerStarting, Workspace: "arco_" + id})
		}))
	}
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.TransitionWorker(running, core.WorkerRunning, 0, core.Event{Kind: "state_change", WorkerID: running})
	}))

	// one pending escalation on the running worker
	var escID string
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		var err error
		escID, err = tx.OpenEscalation(core.Escalation{WorkerID: running, SessionID: session, Kind: "question", Action: "q?"})
		return err
	}))

	// one pool with one active lease
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreatePool(core.ProviderPool{ID: "p1", ClavisProfile: "qwen-1", Provider: "qwen", MaxActive: 5})
	}))
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.AcquireLease(ulid.Make().String(), "p1", time.Minute)
	}))

	got := getStatus(t, ts)
	require.Equal(t, "ok", got.Status)
	require.Equal(t, 1, got.Workers["starting"])
	require.Equal(t, 1, got.Workers["running"])
	require.Equal(t, 1, got.Sessions["open"])

	require.Len(t, got.PendingEscalations, 1)
	esc := got.PendingEscalations[0]
	require.Equal(t, escID, esc.ID)
	require.Equal(t, running, esc.Worker)
	require.Equal(t, "question", esc.Kind)
	require.GreaterOrEqual(t, esc.AgeSeconds, int64(0))
	require.Less(t, esc.AgeSeconds, int64(3600)) // sane, not garbage

	require.Len(t, got.Pools, 1)
	require.Equal(t, "p1", got.Pools[0].ID)
	require.Equal(t, 1, got.Pools[0].ActiveLeases)
	require.Equal(t, 5, got.Pools[0].MaxActive)
}
