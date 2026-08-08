package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestModeCmd_SetsSupervisionMode(t *testing.T) {
	socket, s := startTestDaemon(t)

	session := ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: session, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))

	var out bytes.Buffer
	root := newRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--socket", socket, "mode", session, "manual"})
	require.NoError(t, root.Execute())

	sess, err := s.Reader().GetSession(session)
	require.NoError(t, err)
	require.Equal(t, core.ModeManual, sess.SupervisionMode)
	require.Contains(t, out.String(), "manual", "confirmation names the new mode")
}

func TestModeCmd_InvalidModeRejected(t *testing.T) {
	socket, s := startTestDaemon(t)

	session := ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: session, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))

	root := newRoot()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--socket", socket, "mode", session, "yolo"})
	require.Error(t, root.Execute(), "unknown mode must be rejected")

	sess, err := s.Reader().GetSession(session)
	require.NoError(t, err)
	require.Equal(t, core.ModeAssist, sess.SupervisionMode, "mode unchanged")
}

func TestModeCmd_UnknownSessionErrors(t *testing.T) {
	socket, _ := startTestDaemon(t)
	root := newRoot()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--socket", socket, "mode", "no-such-session", "auto"})
	require.Error(t, root.Execute())
}
