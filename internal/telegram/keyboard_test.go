package telegram

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCallback_RoundTripAllActions(t *testing.T) {
	const escID = "01M0T0HEA2PT43RR9HDGT571F4" // a real ULID length (26)
	for _, a := range []Action{
		ActQuestionOnce, ActQuestionSession, ActQuestionNo,
		ActConfirmYes, ActConfirmNo, ActDiff,
	} {
		data := EncodeCallback(a, escID)
		require.LessOrEqual(t, len(data), 64, "callback_data must fit Telegram's 64-byte cap: %q", data)
		gotA, gotID, err := DecodeCallback(data)
		require.NoError(t, err)
		require.Equal(t, a, gotA)
		require.Equal(t, escID, gotID)
	}
}

func TestDecodeCallback_RejectsUnknownAndMalformed(t *testing.T) {
	bad := []string{
		"",             // empty
		"qo",           // no separator / no id
		"qo:",          // empty id
		"xx:01ABC",     // unknown action
		"rm -rf:01ABC", // injection attempt
		":01ABC",       // empty action
	}
	for _, d := range bad {
		_, _, err := DecodeCallback(d)
		require.Error(t, err, "must reject %q", d)
	}
}

func TestKeyboards_Shape(t *testing.T) {
	const escID = "01M0T0HEA2PT43RR9HDGT571F4"
	q := QuestionKeyboard(escID)
	require.Len(t, q.InlineKeyboard, 2, "question keyboard is 2 rows")
	require.Equal(t, EncodeCallback(ActQuestionOnce, escID), q.InlineKeyboard[0][0].CallbackData)
	require.Equal(t, EncodeCallback(ActQuestionSession, escID), q.InlineKeyboard[0][1].CallbackData)
	require.Equal(t, EncodeCallback(ActQuestionNo, escID), q.InlineKeyboard[1][0].CallbackData)
	require.Equal(t, EncodeCallback(ActDiff, escID), q.InlineKeyboard[1][1].CallbackData)

	c := ConfirmKeyboard(escID)
	require.Equal(t, EncodeCallback(ActConfirmYes, escID), c.InlineKeyboard[0][0].CallbackData)
	require.Equal(t, EncodeCallback(ActConfirmNo, escID), c.InlineKeyboard[0][1].CallbackData)
	require.Equal(t, EncodeCallback(ActDiff, escID), c.InlineKeyboard[1][0].CallbackData)
}
