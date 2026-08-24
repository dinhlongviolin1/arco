package telegram

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// TestLive_InboundRoundTrip exercises the INBOUND relay against REAL Telegram:
// it sends a photo to capture a genuine file_id, then runs the actual
// handleInboundImage (real getFile + DownloadFile + worktree write) and asserts
// a non-empty image landed in the worker's .arco/inbox/. The only inbound step
// it does NOT cover is getUpdates delivering an operator message — identical
// plumbing to the button-tap path already proven live.
//
// Skipped unless ARCO_TG_LIVE=1 with ARCO_TG_TOKEN / ARCO_TG_CHAT / ARCO_TG_TOPIC
// / ARCO_TG_USER set (so `go test ./...` never touches the network).
func TestLive_InboundRoundTrip(t *testing.T) {
	if os.Getenv("ARCO_TG_LIVE") != "1" {
		t.Skip("live telegram test — set ARCO_TG_LIVE=1 + ARCO_TG_* to run")
	}
	token := os.Getenv("ARCO_TG_TOKEN")
	chat, _ := strconv.ParseInt(os.Getenv("ARCO_TG_CHAT"), 10, 64)
	topic, _ := strconv.ParseInt(os.Getenv("ARCO_TG_TOPIC"), 10, 64)
	user, _ := strconv.ParseInt(os.Getenv("ARCO_TG_USER"), 10, 64)
	require.NotEmpty(t, token)
	require.NotZero(t, chat)

	c := NewClient(token, nil)
	ctx := context.Background()

	// make a real PNG and send it (photo compression happens server-side) to get a file_id.
	img := image.NewRGBA(image.Rect(0, 0, 240, 160))
	for y := 0; y < 160; y++ {
		for x := 0; x < 240; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	tmp := filepath.Join(t.TempDir(), "seed.png")
	f, err := os.Create(tmp)
	require.NoError(t, err)
	require.NoError(t, png.Encode(f, img))
	require.NoError(t, f.Close())

	sent, err := c.SendPhoto(ctx, SendPhotoReq{ChatID: chat, MessageThreadID: topic, Path: tmp, Caption: "inbound live-test seed"})
	require.NoError(t, err)
	fileID := sent.LargestPhoto()
	require.NotEmpty(t, fileID, "sendPhoto response carries the stored photo's file_id")

	// wire a Bot to the REAL client + a fake store mapping the topic to a worker worktree.
	wt := t.TempDir()
	st := newFakeStore()
	st.sessions["S1"] = core.Session{ID: "S1", Status: core.SessionActive, TGTopicID: &topic}
	st.workers = []core.Worker{{ID: "W1", OwnerSession: "S1", State: core.WorkerRunning, Worktree: wt}}
	bot := New(Config{API: c, Store: st, GroupID: chat, Actions: &fakeActions{}, Allowed: []int64{user}})

	// run the real inbound handler with a Message carrying the real file_id.
	bot.handleInboundImage(ctx, &Message{
		MessageID: 1, MessageThreadID: topic, From: &User{ID: user},
		Caption: "inbound live-test", Photo: []PhotoSize{{FileID: fileID}},
	})

	entries, err := os.ReadDir(filepath.Join(wt, inboxSubdir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the operator image was downloaded into the worker inbox")
	data, _ := os.ReadFile(filepath.Join(wt, inboxSubdir, entries[0].Name()))
	require.Greater(t, len(data), 100, "a real, non-empty image was written")
	// Telegram serves photos as JPEG; assert a valid image magic (JPEG or PNG).
	require.True(t,
		bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}) || bytes.HasPrefix(data, []byte("\x89PNG")),
		"downloaded bytes are a real image")
}
