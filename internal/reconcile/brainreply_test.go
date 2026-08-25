package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBrainReply_PlainProseIsAValidReply(t *testing.T) {
	e, _, _ := newEngine(t)
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("Hi! You have 0 workers running — use /dispatch to start one."), nil
		}}
	reply, err := e.BrainReply(context.Background(), "hi")
	require.NoError(t, err, "a non-StepResult chat reply is not an error")
	require.Contains(t, reply, "0 workers running")
}

func TestBrainReply_ExecErrorIsAFailure(t *testing.T) {
	e, _, _ := newEngine(t)
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("exit status 1")
		}}
	_, err := e.BrainReply(context.Background(), "hi")
	require.Error(t, err, "a real clavis failure must surface as an error")
}

func TestBrainReply_DisabledBrain(t *testing.T) {
	e, _, _ := newEngine(t)
	e.Brain = BrainCfg{Enabled: false}
	_, err := e.BrainReply(context.Background(), "hi")
	require.Error(t, err)
}
