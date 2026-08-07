package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/api"
	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// Guideline test (rev7/T1.4): `arco escalations` must show the brain's draft,
// its confidence, and its rationale so an operator on a phone can decide from
// the table alone. Do not weaken these asserts.

func startTestDaemon(t *testing.T) (socket string, store *ledger.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := ledger.Open(filepath.Join(dir, "cli.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })

	socket = filepath.Join(dir, "arco.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	srv := &http.Server{Handler: api.New(s, reconcile.New(s, vm.NewFake())).Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return socket, s
}

func TestEscalationsCmd_PrintsDraftConfidenceRationale(t *testing.T) {
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
		_, err := tx.OpenEscalation(core.Escalation{
			WorkerID: worker, SessionID: session, Kind: "question",
			Action:          "which db driver?",
			DraftAnswer:     "use sqlite",
			DraftConfidence: 0.82,
			BrainRationale:  "repo already vendors it",
		})
		return err
	}))

	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--socket", socket, "escalations"})
	require.NoError(t, root.Execute())

	got := out.String()
	// Header must announce the new columns.
	require.Contains(t, got, "DRAFT")
	require.Contains(t, got, "CONF")
	require.Contains(t, got, "RATIONALE")
	// Row must carry the values.
	require.Contains(t, got, "use sqlite")
	require.Contains(t, got, "0.82")
	require.Contains(t, got, "repo already vendors it")
}
