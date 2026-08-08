package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/mergeq"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

func qgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

// newQueueAPI wires a server with the merge queue ENABLED over a seeded
// completed_candidate worker whose worktree clones a bare origin.
func newQueueAPI(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "api.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })

	origin := filepath.Join(t.TempDir(), "origin.git")
	qgit(t, t.TempDir(), "init", "-q", "--bare", "-b", "main", origin)
	wt := filepath.Join(t.TempDir(), "wt")
	qgit(t, t.TempDir(), "clone", "-q", origin, wt)
	qgit(t, wt, "checkout", "-q", "-B", "main")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o644))
	qgit(t, wt, "add", ".")
	qgit(t, wt, "commit", "-q", "-m", "work")
	head := strings.TrimSpace(qgit(t, wt, "rev-parse", "HEAD"))

	sid, wid := ulid.Make().String(), ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sid, Status: core.SessionOpen, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		if err := tx.CreateWorker(core.Worker{
			ID: wid, OwnerSession: sid, State: core.WorkerStarting,
			Workspace: "arco_" + wid, Worktree: wt, HeadCommit: head,
			Task: "t", RunReason: "dispatch",
		}); err != nil {
			return err
		}
		if err := tx.TransitionWorker(wid, core.WorkerRunning, 0, core.Event{Kind: "state_change", WorkerID: wid}); err != nil {
			return err
		}
		return tx.TransitionWorker(wid, core.WorkerCompletedCandidate, 1, core.Event{Kind: "state_change", WorkerID: wid})
	}))

	srv := New(s, reconcile.New(s, vm.NewFake()))
	srv.EnableMergeQueue(mergeq.New(s, mergeq.Config{}))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, wid
}

func TestAPI_QueueDisabledIs503(t *testing.T) {
	ts := newTestAPI(t) // no EnableMergeQueue
	require.Equal(t, http.StatusServiceUnavailable, post(t, ts, "/v1/queue", QueueReq{Worker: "w"}, nil))
	resp, err := http.Get(ts.URL + "/v1/queue")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestAPI_QueueEnqueueAndList(t *testing.T) {
	ts, wid := newQueueAPI(t)

	var enq QueueEnqueueResp
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/queue", QueueReq{Worker: wid}, &enq))
	require.NotEmpty(t, enq.ID)

	// Re-enqueue while pending dedups to the same item id.
	var enq2 QueueEnqueueResp
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/queue", QueueReq{Worker: wid}, &enq2))
	require.Equal(t, enq.ID, enq2.ID)

	resp, err := http.Get(ts.URL + "/v1/queue")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list QueueResp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.Len(t, list.Items, 1)
	require.Equal(t, wid, list.Items[0].Worker)
	require.Equal(t, "pending", list.Items[0].Status)
}

func TestAPI_QueueUnknownWorkerIs404(t *testing.T) {
	ts, _ := newQueueAPI(t)
	require.Equal(t, http.StatusNotFound, post(t, ts, "/v1/queue", QueueReq{Worker: "no-such"}, nil))
	require.Equal(t, http.StatusBadRequest, post(t, ts, "/v1/queue", QueueReq{}, nil))
}
