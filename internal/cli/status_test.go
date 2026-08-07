package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// Guideline tests (rev7/T1.2): `arco status` is the one-screen fleet view an
// operator checks from a phone. It must render workers by state, pending
// escalations with age, and pools; `--json` must emit the raw StatusResp.
// Do not weaken these asserts.

func runStatus(t *testing.T, socket string, extra ...string) string {
	t.Helper()
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--socket", socket, "status"}, extra...))
	require.NoError(t, root.Execute())
	return out.String()
}

func TestStatusCmd_ZeroStateRendersCleanly(t *testing.T) {
	socket, _ := startTestDaemon(t)
	got := strings.ToUpper(runStatus(t, socket))
	require.Contains(t, got, "WORKERS")
	require.Contains(t, got, "ESCALATIONS")
}

func TestStatusCmd_RendersFleetAndJSON(t *testing.T) {
	socket, s := startTestDaemon(t)
	ctx := context.Background()

	session, worker := ulid.Make().String(), ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: session, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateWorker(core.Worker{ID: worker, OwnerSession: session, State: core.WorkerStarting, Workspace: "arco_" + worker})
	}))
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.TransitionWorker(worker, core.WorkerRunning, 0, core.Event{Kind: "state_change", WorkerID: worker})
	}))
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		_, err := tx.OpenEscalation(core.Escalation{WorkerID: worker, SessionID: session, Kind: "question", Action: "pick a driver"})
		return err
	}))

	// human table
	got := runStatus(t, socket)
	require.Contains(t, got, "running")
	require.Contains(t, got, "question")

	// --json: machine-readable, decodes, carries the same facts
	raw := runStatus(t, socket, "--json")
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	workers := m["workers"].(map[string]any)
	require.EqualValues(t, 1, workers["running"])
	require.Len(t, m["pending_escalations"].([]any), 1)
}
