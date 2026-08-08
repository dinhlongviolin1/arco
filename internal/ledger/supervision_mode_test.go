package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// Migration 0006: sessions carry supervision_mode, defaulting to assist (D9).
func TestMigration_SupervisionModeDefaultsAssist(t *testing.T) {
	s := newTestStore(t)
	id := "sess-mode-test"
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: id, Status: core.SessionOpen, Kind: core.SessionKindWork, Title: "t", Goal: "g"})
	}))
	sess, err := s.Reader().GetSession(id)
	require.NoError(t, err)
	require.Equal(t, core.ModeAssist, sess.SupervisionMode, "new sessions default to assist")
}

func TestSetSessionMode_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	id := "sess-mode-test"
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: id, Status: core.SessionOpen, Kind: core.SessionKindWork, Title: "t", Goal: "g"})
	}))

	for _, m := range []core.SupervisionMode{core.ModeManual, core.ModeAuto, core.ModeAssist} {
		require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
			return tx.SetSessionMode(id, m, "operator")
		}))
		sess, err := s.Reader().GetSession(id)
		require.NoError(t, err)
		require.Equal(t, m, sess.SupervisionMode)
	}
}

func TestSetSessionMode_InvalidRejected(t *testing.T) {
	s := newTestStore(t)
	id := "sess-mode-test"
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: id, Status: core.SessionOpen, Kind: core.SessionKindWork, Title: "t", Goal: "g"})
	}))
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetSessionMode(id, core.SupervisionMode("yolo"), "operator")
	})
	require.Error(t, err, "unknown mode must be rejected")
	sess, _ := s.Reader().GetSession(id)
	require.Equal(t, core.ModeAssist, sess.SupervisionMode, "mode unchanged after rejected set")
}

// D9: every mode change is a ledger event attributed to its actor.
func TestSetSessionMode_EventRecordsActor(t *testing.T) {
	s := newTestStore(t)
	id := "sess-mode-test"
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: id, Status: core.SessionOpen, Kind: core.SessionKindWork, Title: "t", Goal: "g"})
	}))
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetSessionMode(id, core.ModeManual, "operator")
	}))

	evs, err := s.Reader().EventsSince(0, 500)
	require.NoError(t, err)
	var found bool
	for _, ev := range evs {
		if ev.Kind == "mode_change" && ev.SessionID == id {
			found = true
			require.Equal(t, "operator", ev.Actor, "mode change must record who did it")
		}
	}
	require.True(t, found, "a mode_change event must be recorded")
}
