package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/redact"
)

// A secret in an event payload must be scrubbed at the write chokepoint, so it
// never lands verbatim in the immutable log.
func TestAppendEvent_ScrubsPayloadAtRest(t *testing.T) {
	s := newTestStore(t)
	s.SetScrubber(redact.New())
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "note", Payload: `{"leak":"` + token + `"}`})
		return err
	}))

	var stored string
	require.NoError(t, s.DB().QueryRow(`SELECT payload FROM events WHERE kind='note'`).Scan(&stored))
	require.NotContains(t, stored, token, "raw secret must not be at rest in the ledger")
	require.Contains(t, stored, "[REDACTED:", "redaction marker present")
}

// A secret in a worker's free-text task must be scrubbed at rest too (F2).
func TestCreateWorker_ScrubsTask(t *testing.T) {
	s := newTestStore(t)
	s.SetScrubber(redact.New())
	sess := newWork(t, s)
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateWorker(core.Worker{ID: "01WSCRUB000000000000000000", OwnerSession: sess,
			State: core.WorkerStarting, Workspace: "arco_x", Task: "push with " + token})
	}))
	var task string
	require.NoError(t, s.DB().QueryRow(`SELECT task FROM workers WHERE id=?`, "01WSCRUB000000000000000000").Scan(&task))
	require.NotContains(t, task, token)
}

// A secret in a session's free-text goal must be scrubbed at rest too — the goal
// is surfaced into the brain prompt via context assembly (qwen review).
func TestCreateSession_ScrubsGoal(t *testing.T) {
	s := newTestStore(t)
	s.SetScrubber(redact.New())
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: "01WSGOAL0000000000000000000", Goal: "ship with " + token, Status: core.SessionActive, Kind: core.SessionKindWork})
	}))
	var goal string
	require.NoError(t, s.DB().QueryRow(`SELECT goal FROM sessions WHERE id=?`, "01WSGOAL0000000000000000000").Scan(&goal))
	require.NotContains(t, goal, token, "raw secret must not be at rest in session.goal")
}
