package client

// T3.4: the hook bridge signs /v1/events with the WORKER's derived key —
// either read from the spawn-time creds-dir file (spawned workers) or derived
// from the configured master + the event's worker ref (local operator hook).
// Verified end-to-end against the real keyed API server over a unix socket.

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/api"
	"github.com/dinhlongviolin1/arco/internal/intakekey"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// keyedServer serves the real API (intake secret set) on a unix socket and
// returns a connected Client plus a dispatched worker's ID.
func keyedServer(t *testing.T, secret string) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := ledger.Open(filepath.Join(dir, "c.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	srv := api.New(s, reconcile.New(s, vm.NewFake()))
	srv.SetIntakeSecret(secret)

	socket := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	hs := &http.Server{Handler: srv.Handler()}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })

	c := New(socket)
	d, err := c.Dispatch(context.Background(), api.DispatchReq{Task: "x", New: true})
	require.NoError(t, err)
	return c, d.WorkerID
}

func TestPostEvent_DerivesPerWorkerKeyFromMaster(t *testing.T) {
	c, workerID := keyedServer(t, "master-secret")

	// Unsigned (no secret, no key) → the server's gate rejects.
	_, err := c.PostEvent(context.Background(), api.EventReq{WorkerRef: workerID, HerdrState: "idle", Alive: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "401")

	// With the master configured, the client signs with the DERIVED worker key.
	c.SetIntakeSecret("master-secret")
	_, err = c.PostEvent(context.Background(), api.EventReq{WorkerRef: workerID, HerdrState: "idle", Alive: true})
	require.NoError(t, err, "master-configured client derives the worker's key")
}

func TestPostEvent_KeyFileWinsOverDerivation(t *testing.T) {
	c, workerID := keyedServer(t, "master-secret")

	// A spawned worker's hook holds only its own key file, never the master.
	c.SetIntakeKey(intakekey.Derive("master-secret", workerID))
	_, err := c.PostEvent(context.Background(), api.EventReq{WorkerRef: workerID, HerdrState: "idle", Alive: true})
	require.NoError(t, err, "the worker's own key file authenticates without a master")

	// A wrong key file must fail even when the right master could derive: the
	// file is authoritative for a spawned worker (no silent fallback).
	c.SetIntakeKey(intakekey.Derive("master-secret", "some-other-worker"))
	c.SetIntakeSecret("master-secret")
	_, err = c.PostEvent(context.Background(), api.EventReq{WorkerRef: workerID, HerdrState: "idle", Alive: true})
	require.Error(t, err)
	require.Contains(t, err.Error(), "401")
}

func TestIntakeKeyFromEnv(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	require.Empty(t, IntakeKeyFromEnv(), "no CREDENTIALS_DIRECTORY → no key")

	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	require.Empty(t, IntakeKeyFromEnv(), "pointer without the intake_key file → no key")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "intake_key"), []byte("abc123\n"), 0o600))
	require.Equal(t, "abc123", IntakeKeyFromEnv(), "file contents, trimmed")
}
