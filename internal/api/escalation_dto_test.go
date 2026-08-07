package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// Guideline test (rev7/T1.4): the escalation DTO must carry the brain's draft
// confidence and rationale so the operator can judge a draft without opening
// the ledger. Do not weaken these asserts.

func seedEscalation(t *testing.T, s *ledger.Store, e core.Escalation) string {
	t.Helper()
	ctx := context.Background()
	session := ulid.Make().String()
	worker := ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: session, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateWorker(core.Worker{ID: worker, OwnerSession: session, State: core.WorkerStarting, Workspace: "arco_" + worker})
	}))
	e.WorkerID, e.SessionID = worker, session
	var id string
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		var err error
		id, err = tx.OpenEscalation(e)
		return err
	}))
	return id
}

func TestEscalationDTO_CarriesDraftConfidenceAndRationale(t *testing.T) {
	s, err := ledger.Open(filepath.Join(t.TempDir(), "api.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(New(s, reconcile.New(s, vm.NewFake())).Handler())
	t.Cleanup(ts.Close)

	id := seedEscalation(t, s, core.Escalation{
		Kind:            "question",
		Action:          "which db driver?",
		DraftAnswer:     "use sqlite",
		DraftConfidence: 0.82,
		BrainRationale:  "repo already vendors mattn/go-sqlite3",
	})

	resp, err := http.Get(ts.URL + "/v1/escalations")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Typed decode: the DTO struct must expose the fields.
	var typed EscalationsResp
	raw := decodeBoth(t, resp, &typed)
	require.Len(t, typed.Escalations, 1)
	got := typed.Escalations[0]
	require.Equal(t, id, got.ID)
	require.Equal(t, "use sqlite", got.Draft)
	require.InDelta(t, 0.82, got.DraftConfidence, 1e-9)
	require.Equal(t, "repo already vendors mattn/go-sqlite3", got.BrainRationale)

	// Wire contract: exact JSON keys, so phone cards / jq scripts can rely on them.
	list := raw["escalations"].([]any)
	require.Len(t, list, 1)
	obj := list[0].(map[string]any)
	require.Contains(t, obj, "draft_confidence")
	require.Contains(t, obj, "brain_rationale")
	require.InDelta(t, 0.82, obj["draft_confidence"].(float64), 1e-9)
	require.Equal(t, "repo already vendors mattn/go-sqlite3", obj["brain_rationale"])
}

// A draftless escalation must serialize zero values, not omit-crash or lie.
func TestEscalationDTO_NilDraftZeroValues(t *testing.T) {
	s, err := ledger.Open(filepath.Join(t.TempDir(), "api.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	ts := httptest.NewServer(New(s, reconcile.New(s, vm.NewFake())).Handler())
	t.Cleanup(ts.Close)

	seedEscalation(t, s, core.Escalation{Kind: "question", Action: "no draft yet"})

	resp, err := http.Get(ts.URL + "/v1/escalations")
	require.NoError(t, err)
	defer resp.Body.Close()
	var typed EscalationsResp
	decodeBoth(t, resp, &typed)
	require.Len(t, typed.Escalations, 1)
	require.Zero(t, typed.Escalations[0].DraftConfidence)
	require.Empty(t, typed.Escalations[0].BrainRationale)
}

// decodeBoth decodes the response body into out and also returns the raw map.
func decodeBoth(t *testing.T, resp *http.Response, out any) map[string]any {
	t.Helper()
	var buf json.RawMessage
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&buf))
	require.NoError(t, json.Unmarshal(buf, out))
	m := map[string]any{}
	require.NoError(t, json.Unmarshal(buf, &m))
	return m
}
