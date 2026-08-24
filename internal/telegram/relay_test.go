package telegram

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestSendSessionImage_PostsToTopic(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	seedSession(st, "S1", "s1")
	mid, err := b.SendSessionImage(context.Background(), "S1", "/tmp/shot.png", "look")
	require.NoError(t, err)
	require.NotZero(t, mid)
	require.Len(t, api.photos, 1)
	require.Equal(t, "/tmp/shot.png", api.photos[0].Path)
	require.Equal(t, "look", api.photos[0].Caption)
	require.NotZero(t, api.photos[0].MessageThreadID, "sent into the session topic")
}

func TestInboundImage_DownloadsIntoWorkerWorktree(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	// session S1 already bound to topic 7; a running worker with a real worktree
	wt := t.TempDir()
	topic := int64(7)
	st.sessions["S1"] = core.Session{ID: "S1", Slug: "s1", Status: core.SessionActive, TGTopicID: &topic}
	st.workers = []core.Worker{{ID: "W1", OwnerSession: "S1", State: core.WorkerRunning, Worktree: wt}}

	// operator (allowed) posts a photo in the topic
	b.handleInboundImage(context.Background(), &Message{
		MessageID: 3, MessageThreadID: topic, From: &User{ID: allowedUID},
		Caption: "the bug", Photo: []PhotoSize{{FileID: "small"}, {FileID: "big", Width: 1280}},
	})

	// downloaded into <worktree>/.arco/inbox/
	inbox := filepath.Join(wt, inboxSubdir)
	entries, err := os.ReadDir(inbox)
	require.NoError(t, err)
	require.Len(t, entries, 1, "one image written to the worker inbox")
	data, _ := os.ReadFile(filepath.Join(inbox, entries[0].Name()))
	require.Equal(t, "IMAGEBYTES", string(data))
	// confirmation posted into the topic mentioning the saved path + caption
	var confirmed bool
	for _, m := range api.sent {
		if m.thread == topic && containsAll(m.text, ".arco/inbox", "the bug") {
			confirmed = true
		}
	}
	require.True(t, confirmed, "topic gets a confirmation with the saved path + caption")
}

func TestInboundImage_UnauthorizedDropped(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	wt := t.TempDir()
	topic := int64(7)
	st.sessions["S1"] = core.Session{ID: "S1", Status: core.SessionActive, TGTopicID: &topic}
	st.workers = []core.Worker{{ID: "W1", OwnerSession: "S1", State: core.WorkerRunning, Worktree: wt}}

	b.handleInboundImage(context.Background(), &Message{
		MessageID: 3, MessageThreadID: topic, From: &User{ID: 999}, // not allowed
		Photo: []PhotoSize{{FileID: "big"}},
	})
	entries, _ := os.ReadDir(filepath.Join(wt, inboxSubdir))
	require.Empty(t, entries, "a stranger's image must not be downloaded")
	require.Empty(t, api.sent, "and nothing echoed")
}

func TestInboundImage_NoWorktreeIsGraceful(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	topic := int64(7)
	st.sessions["S1"] = core.Session{ID: "S1", Status: core.SessionActive, TGTopicID: &topic}
	// no workers for the session
	b.handleInboundImage(context.Background(), &Message{
		MessageID: 3, MessageThreadID: topic, From: &User{ID: allowedUID},
		Photo: []PhotoSize{{FileID: "big"}},
	})
	require.NotEmpty(t, api.sent, "posts a 'no active worker' note rather than erroring")
	require.Contains(t, api.sent[len(api.sent)-1].text, "no active worker")
}

func TestSanitizeName_NoTraversal(t *testing.T) {
	require.Equal(t, "image", sanitizeName(""))
	require.Equal(t, "shot.png", sanitizeName("shot.png"))
	require.NotContains(t, sanitizeName("../../etc/passwd"), "..")
	require.Equal(t, "passwd", sanitizeName("/etc/passwd"))
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
