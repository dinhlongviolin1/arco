package api

// rev7 T3.5: GET /v1/autonomy — the earn-out report behind `arco autonomy`.

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

// seedDraftedDecision records one human answer to a drafted clarify question.
func seedDraftedDecision(t *testing.T, s *ledger.Store) {
	t.Helper()
	sid, wid := ulid.Make().String(), ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sid, Status: core.SessionOpen, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		return tx.CreateWorker(core.Worker{ID: wid, OwnerSession: sid, State: core.WorkerStarting, Workspace: "arco_" + wid})
	}))
	var escID string
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.TransitionWorker(wid, core.WorkerRunning, 0, core.Event{Kind: "state_change", WorkerID: wid}); err != nil {
			return err
		}
		w, err := tx.GetWorker(wid)
		if err != nil {
			return err
		}
		if err := tx.TransitionWorker(wid, core.WorkerWaitingForUser, w.Rev, core.Event{Kind: "state_change", WorkerID: wid}); err != nil {
			return err
		}
		escID, err = tx.OpenEscalation(core.Escalation{
			WorkerID: wid, SessionID: sid, Kind: "question", QuestionClass: "clarify",
			Action: "step?", DraftAnswer: "plan A",
		})
		return err
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.AnswerQuestion(escID, "plan A", core.ScopeOnce, core.Event{Kind: "question_ans"})
	}))
}

func TestAPI_AutonomyReport(t *testing.T) {
	s, err := ledger.Open(filepath.Join(t.TempDir(), "api.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	eng := reconcile.New(s, vm.NewFake())
	eng.VerificationLive = true
	eng.EarnOutMinDecisions = 1
	eng.EarnOutMinAgreement = 1.0
	ts := httptest.NewServer(New(s, eng).Handler())
	t.Cleanup(ts.Close)

	seedDraftedDecision(t, s)

	resp, err := http.Get(ts.URL + "/v1/autonomy")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out AutonomyResp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.True(t, out.VerificationLive)
	require.Equal(t, 1, out.MinDecisions)
	require.InDelta(t, 1.0, out.MinAgreement, 1e-9)
	require.Len(t, out.Classes, 5, "one row per frozen question_class")
	byClass := map[string]AutonomyClassDTO{}
	for _, c := range out.Classes {
		byClass[c.Class] = c
	}
	require.Equal(t, 1, byClass["clarify"].Agree)
	require.Equal(t, 1, byClass["clarify"].Total)
	require.True(t, byClass["clarify"].Promotes)
	require.False(t, byClass["other"].Promotes)
}
