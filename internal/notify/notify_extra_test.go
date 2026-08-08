package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/containrrr/shoutrrr/pkg/types"
	"github.com/stretchr/testify/require"
)

// Level.String round-trips through ParseLevel for every defined level.
func TestLevelStringRoundTrip(t *testing.T) {
	for _, l := range []Level{LevelInfo, LevelWarn, LevelUrgent} {
		got, err := ParseLevel(l.String())
		require.NoError(t, err)
		require.Equal(t, l, got)
	}
}

// ParseLevel names the offending input in its error.
func TestParseLevel_UnknownNamesInput(t *testing.T) {
	_, err := ParseLevel("loud")
	require.ErrorContains(t, err, "loud")
}

// New defers a bad shoutrrr URL to Send (no panic, no error return from New).
func TestNew_InvalidURL_ErrorsOnSend(t *testing.T) {
	s := New([]string{"definitely-not-a-service://x"}, LevelInfo)
	err := s.Send(context.Background(), Card{Level: LevelUrgent, Title: "t", Body: "b"})
	require.Error(t, err)
}

// The min-level filter drops cards BEFORE a broken service is ever touched.
func TestNew_InvalidURL_FilterStillDrops(t *testing.T) {
	s := New([]string{"definitely-not-a-service://x"}, LevelWarn)
	require.NoError(t, s.Send(context.Background(), Card{Level: LevelInfo, Title: "t", Body: "b"}))
}

// routerSender joins per-service errors into one error.
func TestRouterSender_JoinsServiceErrors(t *testing.T) {
	rs := routerSender{send: func(message string, params *types.Params) []error {
		return []error{errors.New("boom a"), nil, errors.New("boom b")}
	}}
	err := rs.Send(context.Background(), Card{Title: "t", Body: "b"})
	require.ErrorContains(t, err, "boom a")
	require.ErrorContains(t, err, "boom b")
}

// routerSender honors the card body + title param, and a cancelled ctx.
func TestRouterSender_BodyAndTitleAndCtx(t *testing.T) {
	var gotMsg string
	var gotParams *types.Params
	rs := routerSender{send: func(message string, params *types.Params) []error {
		gotMsg, gotParams = message, params
		return nil
	}}
	require.NoError(t, rs.Send(context.Background(), Card{Title: "T", Body: "B"}))
	require.Equal(t, "B", gotMsg)
	require.Equal(t, "T", (*gotParams)["title"])

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, rs.Send(ctx, Card{}), context.Canceled)
}
