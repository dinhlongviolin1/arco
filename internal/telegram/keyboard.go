package telegram

import (
	"fmt"
	"strings"
)

// Action is the operator intent encoded in a button's callback_data. Kept to a
// 2-char token so callback_data (action + ":" + escID) stays well under
// Telegram's 64-byte cap: a ULID escalation id is 26 bytes, so a payload is
// ~29 bytes.
type Action string

const (
	ActQuestionOnce    Action = "qo" // answer a question with the brain draft, scope=once
	ActQuestionSession Action = "qs" // answer a question with the brain draft, scope=session
	ActQuestionNo      Action = "qn" // reject a question ("no")
	ActConfirmYes      Action = "cy" // approve a confirm
	ActConfirmNo       Action = "cn" // reject a confirm
	ActDiff            Action = "df" // show the worker's redacted diff
)

var validActions = map[Action]bool{
	ActQuestionOnce: true, ActQuestionSession: true, ActQuestionNo: true,
	ActConfirmYes: true, ActConfirmNo: true, ActDiff: true,
}

// EncodeCallback builds the callback_data for a button: "<action>:<escID>".
func EncodeCallback(a Action, escID string) string {
	return string(a) + ":" + escID
}

// DecodeCallback parses callback_data produced by EncodeCallback. An unknown
// action or malformed payload is an error (the inbound loop drops it) — a
// stranger or a stale client can never smuggle an arbitrary command through.
func DecodeCallback(data string) (Action, string, error) {
	a, escID, ok := strings.Cut(data, ":")
	if !ok || escID == "" {
		return "", "", fmt.Errorf("telegram: malformed callback data %q", data)
	}
	act := Action(a)
	if !validActions[act] {
		return "", "", fmt.Errorf("telegram: unknown callback action %q", a)
	}
	return act, escID, nil
}

// QuestionKeyboard is the inline keyboard for a QUESTION escalation:
//
//	[ ✅ once ] [ ✅ always ]
//	[  ❌ no  ] [ 👀 diff  ]
//
// "once" / "always" both accept the brain's DRAFT answer (the operator's tap is
// the human decision the earn-out needs); "always" additionally promotes a
// standing session grant (scope=session).
func QuestionKeyboard(escID string) InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{
			{Text: "✅ once", CallbackData: EncodeCallback(ActQuestionOnce, escID)},
			{Text: "✅ always", CallbackData: EncodeCallback(ActQuestionSession, escID)},
		},
		{
			{Text: "❌ no", CallbackData: EncodeCallback(ActQuestionNo, escID)},
			{Text: "👀 diff", CallbackData: EncodeCallback(ActDiff, escID)},
		},
	}}
}

// ConfirmKeyboard is the inline keyboard for a CONFIRM (danger-class) escalation:
//
//	[ ✅ approve ] [ ❌ reject ]
//	[   👀 diff  ]
func ConfirmKeyboard(escID string) InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{
			{Text: "✅ approve", CallbackData: EncodeCallback(ActConfirmYes, escID)},
			{Text: "❌ reject", CallbackData: EncodeCallback(ActConfirmNo, escID)},
		},
		{
			{Text: "👀 diff", CallbackData: EncodeCallback(ActDiff, escID)},
		},
	}}
}
