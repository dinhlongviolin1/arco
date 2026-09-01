package telegram

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

type appendRec struct{ sid, role, content string }

type fakeCStore struct {
	appended []appendRec
	byS      map[string][]ContextMessage
}

func newFakeCStore() *fakeCStore { return &fakeCStore{byS: map[string][]ContextMessage{}} }

func (f *fakeCStore) AppendMessage(_ context.Context, sid, role, content string) error {
	f.appended = append(f.appended, appendRec{sid, role, content})
	f.byS[sid] = append(f.byS[sid], ContextMessage{Role: role, Content: content})
	return nil
}
func (f *fakeCStore) RecentMessages(sid string, limit int) ([]ContextMessage, error) {
	msgs := f.byS[sid]
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return msgs, nil
}

func newBotWithStore(cs ContextStore) (*Bot, *fakeAPIRec, *fakeStore, *fakeActions) {
	api := &fakeAPIRec{}
	st := newFakeStore()
	act := &fakeActions{}
	b := New(Config{
		API: api, Store: st, GroupID: -100999, MinLevel: notify.LevelInfo,
		Actions: act, Allowed: []int64{allowedUID}, Redact: fakeScrubber{}, ContextStore: cs,
	})
	return b, api, st, act
}

// A General-topic chat turn persists durably to the console sentinel session, and
// the next turn's brain prompt carries the prior turn from the durable store.
func TestChat_DurableHistoryGeneralTopic(t *testing.T) {
	cs := newFakeCStore()
	b, _, _, act := newBotWithStore(cs)
	act.chatReply = "2 workers are running"

	b.handleMessage(context.Background(), &Message{Text: "what's running?", From: &User{ID: allowedUID}})

	require.Len(t, cs.appended, 2, "operator + arco turns persisted")
	require.Equal(t, appendRec{core.ConsoleSessionID, "operator", "what's running?"}, cs.appended[0])
	require.Equal(t, appendRec{core.ConsoleSessionID, "arco", "2 workers are running"}, cs.appended[1])

	// Second turn: the prompt includes the durable first turn.
	b.handleMessage(context.Background(), &Message{Text: "and now?", From: &User{ID: allowedUID}})
	require.Len(t, act.chatPrompts, 2)
	require.Contains(t, act.chatPrompts[1], "what's running?", "prior operator turn is in durable context")
	require.Contains(t, act.chatPrompts[1], "2 workers are running", "prior arco turn is in durable context")
}

// Chat inside an issue topic keys durable history to THAT session, not the console.
func TestChat_DurableHistoryIssueTopic(t *testing.T) {
	cs := newFakeCStore()
	b, _, st, act := newBotWithStore(cs)
	act.chatReply = "ok"
	topic := int64(7)
	st.sessions["ISSUE9"] = core.Session{ID: "ISSUE9", Status: core.SessionActive, TGTopicID: &topic}

	b.handleMessage(context.Background(), &Message{Text: "status?", From: &User{ID: allowedUID}, MessageThreadID: topic})
	require.Len(t, cs.appended, 2)
	require.Equal(t, "ISSUE9", cs.appended[0].sid, "durable history keyed to the topic's session")
	require.Equal(t, "ISSUE9", cs.appended[1].sid)
}

// A fresh session with no durable history renders "none yet" (not a crash).
func TestChat_DurableHistoryEmpty(t *testing.T) {
	b, _, _, _ := newBotWithStore(newFakeCStore())
	require.Equal(t, "none yet", b.chatHistory(0))
}
