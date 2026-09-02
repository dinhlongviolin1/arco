package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/feature"
)

// pendingAction is a brain-proposed mutating action awaiting the operator's ✅.
// It captures the pure executor (the tool) + its args + where the card lives, so
// a later button tap can run it exactly as proposed.
type pendingAction struct {
	tool   feature.Tool
	args   json.RawMessage
	desc   string
	thread int64 // the topic the proposal was made in
	msgID  int64 // the card message id (to edit on resolve)
	seq    int64 // creation order, for oldest-first eviction
}

// maxPending caps the outstanding proposal store so a brain that keeps proposing
// actions the operator ignores can't grow it without bound (entries are tiny;
// this is a slow-leak guard). Oldest proposals are evicted first.
const maxPending = 64

// gateForThread builds the gate the tool-loop uses for mutating tools in a given
// topic: the mode comes from the operator's config (default confirm); confirm-mode
// proposals post an approval card in THAT topic. Used for both chat turns and
// scheduled-task runs.
func (b *Bot) gateForThread(threadID int64) feature.Gate {
	return feature.Gate{
		Mode: func(name string) feature.Mode {
			if b.fmode == nil {
				return feature.ModeConfirm // safe default: propose, don't act
			}
			return b.fmode(name)
		},
		Confirm: func(ctx context.Context, t feature.Tool, args json.RawMessage) (string, error) {
			return b.proposeAction(ctx, threadID, t, args)
		},
	}
}

// chatGate is the gate for a chat turn — confirmations post in the message's topic.
func (b *Bot) chatGate(m *Message) feature.Gate { return b.gateForThread(m.MessageThreadID) }

// gateForScheduled is the gate for an UNATTENDED scheduled run. It is stricter than
// the interactive gate: a mutating capability the operator set to `auto` is clamped
// to `confirm` here, because no one is watching a 3am run — an autonomous kill/
// dispatch must still post an approval card and wait for a ✅. `off` (never) and
// `confirm` are honored as-is; only `auto` is downgraded.
func (b *Bot) gateForScheduled(threadID int64) feature.Gate {
	g := b.gateForThread(threadID)
	base := g.Mode
	g.Mode = func(name string) feature.Mode {
		if base(name) == feature.ModeAuto {
			return feature.ModeConfirm
		}
		return base(name)
	}
	return g
}

// proposeAction records a pending mutating action and posts a ✅/❌ card in the
// given topic, returning the text to relay back to the model so its reply tells
// the operator to approve.
func (b *Bot) proposeAction(ctx context.Context, threadID int64, t feature.Tool, args json.RawMessage) (string, error) {
	desc := describeAction(t.Name, args)

	b.mu.Lock()
	b.pendSeq++
	seq := b.pendSeq
	id := strconv.FormatInt(seq, 36) // short, fits the 64-byte callback cap
	b.pending[id] = pendingAction{tool: t, args: args, desc: desc, thread: threadID, seq: seq}
	b.evictOldestPendingLocked()
	b.mu.Unlock()

	kb := ApproveKeyboard(id)
	sent, err := b.api.SendMessage(ctx, SendMessageReq{
		ChatID: b.groupID, MessageThreadID: threadID,
		Text: b.scrub("⚠️ arco wants to: " + desc + "\nApprove?"), ReplyMarkup: &kb,
	})
	if err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return "", err
	}
	b.mu.Lock()
	if pa, ok := b.pending[id]; ok { // may have been evicted under heavy load
		pa.msgID = sent.MessageID
		b.pending[id] = pa
	}
	b.mu.Unlock()

	return "I've asked the operator to approve: " + desc + " — a card with ✅/❌ is posted; it runs only if they approve.", nil
}

// evictOldestPendingLocked drops the oldest proposal(s) once the store exceeds
// maxPending. Caller holds b.mu.
func (b *Bot) evictOldestPendingLocked() {
	for len(b.pending) > maxPending {
		var oldestID string
		var oldestSeq int64
		for id, pa := range b.pending {
			if oldestID == "" || pa.seq < oldestSeq {
				oldestID, oldestSeq = id, pa.seq
			}
		}
		delete(b.pending, oldestID)
	}
}

// resolvePending handles an approve/reject tap on a proposed action's card. On
// approve it executes the tool exactly as proposed (approval == run); on reject
// it just clears the card. Returns the toast text.
func (b *Bot) resolvePending(ctx context.Context, act Action, id string) string {
	b.mu.Lock()
	pa, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
	}
	b.mu.Unlock()
	if !ok {
		return "already handled (or expired)"
	}

	if act == ActReject {
		b.editPendingCard(ctx, pa, "❌ rejected — "+pa.desc+" (not run)")
		return "rejected"
	}
	// Approve → run the pure executor (the loop's policy gate is bypassed here on
	// purpose: the operator's tap IS the authorization). Underlying estop/permission
	// checks still apply inside the engine call.
	out, err := pa.tool.Call(ctx, pa.args)
	if err != nil {
		b.editPendingCard(ctx, pa, "⚠️ approved but failed: "+err.Error())
		return "failed: " + err.Error()
	}
	b.editPendingCard(ctx, pa, "✅ approved — "+b.scrub(out))
	return "approved"
}

// editPendingCard replaces a proposal card's text and strips its buttons.
func (b *Bot) editPendingCard(ctx context.Context, pa pendingAction, text string) {
	if pa.msgID == 0 {
		_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: pa.thread, Text: text})
		return
	}
	empty := InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}
	err := b.api.EditMessageText(ctx, EditMessageTextReq{
		ChatID: b.groupID, MessageID: pa.msgID, Text: truncate(text, tgMessageCap), ReplyMarkup: &empty,
	})
	if err != nil && !IsNotModified(err) {
		_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: pa.thread, Text: text})
	}
}

// describeAction renders a short human description of a proposed tool call for the
// approval card, e.g. `kill worker=w3` or `adopt ref=w1:p1`.
func describeAction(name string, args json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil || len(m) == 0 {
		return name
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	// stable-ish order: keep it simple (map order is fine for a one-line label)
	return name + " " + strings.Join(parts, " ")
}
