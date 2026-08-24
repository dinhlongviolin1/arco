package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScrubToken_Direct(t *testing.T) {
	c := NewClient("SEKRET-TOKEN", nil)
	require.Equal(t, "boom *** end", c.scrubToken(errors.New("boom SEKRET-TOKEN end")).Error())
	require.Nil(t, c.scrubToken(nil))
	require.Equal(t, "no token here", c.scrubToken(errors.New("no token here")).Error())
}

// A transport failure (connection refused) must NOT surface the bot token, which
// is embedded in the request URL — the leak the review caught.
func TestClient_TransportErrorScrubsToken(t *testing.T) {
	const tok = "7737662158:AAHsupersecretbodyxxxxxxxxxxxxxxxxxx"
	c := NewClient(tok, nil)
	c.base = "http://127.0.0.1:1" // nothing listens → connection refused, error carries the URL
	_, err := c.SendMessage(context.Background(), SendMessageReq{ChatID: 1, Text: "x"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), tok, "bot token must never appear in a transport error")
	require.Contains(t, err.Error(), "***")
}

// fakeAPI is an httptest Bot API: it records the last method+body and replies
// with a canned ok envelope (or a canned error).
type fakeAPI struct {
	srv        *httptest.Server
	lastMethod string
	lastBody   map[string]any
	reply      map[string]any // per-method canned "result"
	fail       *APIError      // if set, every call returns this error envelope
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{reply: map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path is /bot<token>/<method>
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		f.lastMethod = parts[len(parts)-1]
		b, _ := io.ReadAll(r.Body)
		f.lastBody = map[string]any{}
		_ = json.Unmarshal(b, &f.lastBody)
		w.Header().Set("Content-Type", "application/json")
		if f.fail != nil {
			_ = json.NewEncoder(w).Encode(apiResponse{OK: false, ErrorCode: f.fail.Code, Description: f.fail.Description})
			return
		}
		res, _ := json.Marshal(f.reply[f.lastMethod])
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: res})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) client() *Client {
	c := NewClient("TESTTOKEN", f.srv.Client())
	c.base = f.srv.URL
	return c
}

func TestClient_SendMessage(t *testing.T) {
	f := newFakeAPI(t)
	f.reply["sendMessage"] = Message{MessageID: 42, Chat: Chat{ID: -100999}}
	c := f.client()
	m, err := c.SendMessage(context.Background(), SendMessageReq{
		ChatID: -100999, MessageThreadID: 7, Text: "hi",
		ReplyMarkup: &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: "x", CallbackData: "qo:1"}}}},
	})
	require.NoError(t, err)
	require.Equal(t, int64(42), m.MessageID)
	require.Equal(t, "sendMessage", f.lastMethod)
	require.EqualValues(t, -100999, f.lastBody["chat_id"])
	require.EqualValues(t, 7, f.lastBody["message_thread_id"])
	require.Contains(t, f.lastBody, "reply_markup")
}

func TestClient_CreateForumTopic(t *testing.T) {
	f := newFakeAPI(t)
	f.reply["createForumTopic"] = map[string]any{"message_thread_id": 555}
	c := f.client()
	id, err := c.CreateForumTopic(context.Background(), -100999, "session: fix-auth")
	require.NoError(t, err)
	require.Equal(t, int64(555), id)
	require.Equal(t, "session: fix-auth", f.lastBody["name"])
}

func TestClient_GetUpdatesParsesBothKinds(t *testing.T) {
	f := newFakeAPI(t)
	f.reply["getUpdates"] = []Update{
		{UpdateID: 1, Message: &Message{MessageID: 9, Text: "/status", MessageThreadID: 0, From: &User{ID: 573409113}}},
		{UpdateID: 2, CallbackQuery: &CallbackQuery{ID: "cbq1", Data: "qo:01ABC", From: User{ID: 573409113}}},
	}
	c := f.client()
	ups, err := c.GetUpdates(context.Background(), 0, 1)
	require.NoError(t, err)
	require.Len(t, ups, 2)
	require.Equal(t, "/status", ups[0].Message.Text)
	require.Equal(t, "qo:01ABC", ups[1].CallbackQuery.Data)
	require.Equal(t, int64(573409113), ups[1].CallbackQuery.From.ID)
}

func TestClient_APIErrorSurfacesDescription(t *testing.T) {
	f := newFakeAPI(t)
	f.fail = &APIError{Code: 400, Description: "Bad Request: chat not found"}
	c := f.client()
	_, err := c.SendMessage(context.Background(), SendMessageReq{ChatID: 1, Text: "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "chat not found")
}

func TestClient_IsNotModifiedBenign(t *testing.T) {
	f := newFakeAPI(t)
	f.fail = &APIError{Code: 400, Description: "Bad Request: message is not modified"}
	c := f.client()
	err := c.EditMessageText(context.Background(), EditMessageTextReq{ChatID: 1, MessageID: 2, Text: "same"})
	require.Error(t, err)
	require.True(t, IsNotModified(err), "unchanged-edit error must be recognized as benign")
}
