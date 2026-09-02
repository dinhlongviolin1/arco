package telegram

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/feature"
)

func mutatingTool(ran *int, gotArgs *string, out string) feature.Tool {
	return feature.Tool{Name: "kill", Desc: "kill a worker", Access: feature.BrainAct,
		Call: func(_ context.Context, args json.RawMessage) (string, error) {
			*ran++
			*gotArgs = string(args)
			return out, nil
		}}
}

func pendingID(b *Bot) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.pending {
		return k
	}
	return ""
}

// Proposing a mutating action posts a ✅/❌ card, stores it pending, and does NOT
// execute — the brain-facing return says approval is required.
func TestConfirm_ProposePostsCardAndDefers(t *testing.T) {
	b, api, _ := newTestBotReg(t, feature.NewRegistry())
	var ran int
	var args string
	m := &Message{MessageThreadID: 7, From: &User{ID: allowedUID}}
	msg, err := b.proposeAction(context.Background(), m.MessageThreadID, mutatingTool(&ran, &args, "killed w3"), []byte(`{"worker":"w3"}`))
	require.NoError(t, err)
	require.Contains(t, msg, "approve")
	require.Contains(t, msg, "kill worker=w3", "the card describes the concrete action")
	require.Equal(t, 0, ran, "proposing never executes")

	last := api.sent[len(api.sent)-1]
	require.Equal(t, int64(7), last.thread, "card posted in the message's topic")
	require.Contains(t, last.text, "arco wants to")
	require.NotNil(t, last.keyboard, "card carries the approve/reject keyboard")
}

// Approving runs the tool exactly as proposed and edits the card.
func TestConfirm_ApproveExecutes(t *testing.T) {
	b, api, _ := newTestBotReg(t, feature.NewRegistry())
	var ran int
	var args string
	m := &Message{MessageThreadID: 7}
	_, _ = b.proposeAction(context.Background(), m.MessageThreadID, mutatingTool(&ran, &args, "🛑 killed w3"), []byte(`{"worker":"w3"}`))
	id := pendingID(b)
	require.NotEmpty(t, id)

	toast := b.resolvePending(context.Background(), ActApprove, id)
	require.Equal(t, "approved", toast)
	require.Equal(t, 1, ran, "approve executes the tool")
	require.Contains(t, args, "w3", "the tool ran with the proposed args")
	require.Contains(t, api.edits[len(api.edits)-1].Text, "✅ approved")
	require.Empty(t, pendingID(b), "pending cleared after resolution")
}

// Rejecting never executes; the card is marked rejected.
func TestConfirm_RejectDoesNotExecute(t *testing.T) {
	b, api, _ := newTestBotReg(t, feature.NewRegistry())
	var ran int
	var args string
	_, _ = b.proposeAction(context.Background(), int64(7), mutatingTool(&ran, &args, "x"), []byte(`{"worker":"w3"}`))
	id := pendingID(b)
	toast := b.resolvePending(context.Background(), ActReject, id)
	require.Equal(t, "rejected", toast)
	require.Equal(t, 0, ran, "reject must not run the tool")
	require.Contains(t, api.edits[len(api.edits)-1].Text, "❌ rejected")
}

// A double-tap / stale id is handled gracefully.
func TestConfirm_UnknownPendingID(t *testing.T) {
	b, _, _ := newTestBotReg(t, feature.NewRegistry())
	require.Equal(t, "already handled (or expired)", b.resolvePending(context.Background(), ActApprove, "nope"))
}

// The approve tap routes through the real callback handler and executes.
func TestConfirm_ApproveViaCallback(t *testing.T) {
	b, _, _ := newTestBotReg(t, feature.NewRegistry())
	var ran int
	var args string
	_, _ = b.proposeAction(context.Background(), int64(7), mutatingTool(&ran, &args, "done"), []byte(`{"worker":"w3"}`))
	id := pendingID(b)
	b.handleCallback(context.Background(), &CallbackQuery{
		ID: "c1", From: User{ID: allowedUID}, Data: EncodeCallback(ActApprove, id),
	})
	require.Equal(t, 1, ran, "an approve callback executes the proposed action")
}

// chatGate defaults to confirm when no policy is configured (safe by default).
func TestConfirm_GateDefaultsToConfirm(t *testing.T) {
	b, _, _ := newTestBotReg(t, feature.NewRegistry())
	g := b.chatGate(&Message{MessageThreadID: 1})
	require.Equal(t, feature.ModeConfirm, g.Mode("kill"))
}

// The pending store is bounded — an ignored flood of proposals can't grow it
// without limit (oldest are evicted).
func TestConfirm_PendingBounded(t *testing.T) {
	b, _, _ := newTestBotReg(t, feature.NewRegistry())
	var ran int
	var args string
	tool := mutatingTool(&ran, &args, "ok")
	for i := 0; i < maxPending+20; i++ {
		_, _ = b.proposeAction(context.Background(), int64(1), tool, []byte(`{"worker":"w"}`))
	}
	b.mu.Lock()
	n := len(b.pending)
	b.mu.Unlock()
	require.LessOrEqual(t, n, maxPending, "pending store is capped")
	require.Equal(t, 0, ran, "proposing never executes")
}

func TestDescribeAction(t *testing.T) {
	require.Equal(t, "scan", describeAction("scan", nil))
	require.Equal(t, "scan", describeAction("scan", []byte(`{}`)))
	require.Contains(t, describeAction("kill", []byte(`{"worker":"w3"}`)), "worker=w3")
}
