package telegram

import (
	"context"
	"fmt"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// scheduledPreamble frames an unattended scheduled run.
const scheduledPreamble = `You are arco running a SCHEDULED task unattended. Use the read-only tools to gather live fleet state; be concise and factual, and do not invent details. If a fix requires a mutating action (kill/adopt/dispatch) you may propose it — it will require the operator's approval before it runs. Report what you found (and proposed).`

// RunScheduledTask executes one recurring task as an AGENTIC run in the task's own
// topic + session: the chat brain (Converse) with the full tool surface (mutating
// tools gated by the operator's confirm policy — a proposal posts an approval card
// in the task's topic) and the task's durable memory (so it recalls prior runs).
// The run's output is posted as a card in the task's topic and appended to the
// task's memory; a short line is returned for the ledger.
func (b *Bot) RunScheduledTask(ctx context.Context, task core.ScheduledTask) (string, error) {
	if b.reg == nil {
		return "", fmt.Errorf("scheduled run: no feature registry")
	}
	topicID, err := b.ensureTopic(ctx, task.SessionID)
	if err != nil {
		return "", fmt.Errorf("scheduled run: open topic: %w", err)
	}
	out, err := b.actions.Converse(ctx, scheduledPreamble, task.Prompt, task.SessionID,
		b.reg.ForBrain(), b.gateForScheduled(topicID))
	if err != nil {
		return "", err
	}
	out = b.scrub(out)

	// Persist the run into the task's own memory so future runs recall it.
	if b.cstore != nil {
		_ = b.cstore.AppendMessage(ctx, task.SessionID, "arco", out)
	}
	// Post the result as a card in the task's topic.
	_, _ = b.api.SendMessage(ctx, SendMessageReq{
		ChatID: b.groupID, MessageThreadID: topicID,
		Text: truncate("⏰ "+task.Name+" —\n"+out, tgMessageCap),
	})
	return truncate(out, 180), nil
}
