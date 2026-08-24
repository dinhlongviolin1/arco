package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLargestPhoto(t *testing.T) {
	m := &Message{Photo: []PhotoSize{{FileID: "small", Width: 90}, {FileID: "big", Width: 1280}}}
	require.Equal(t, "big", m.LargestPhoto(), "picks the last (largest) size")
	require.Empty(t, (&Message{}).LargestPhoto(), "no photo → empty")
}

func TestGetFileAndDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			w.Header().Set("Content-Type", "application/json")
			res, _ := json.Marshal(File{FileID: "abc", FilePath: "photos/file_1.jpg"})
			_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: res})
		case strings.Contains(r.URL.Path, "/file/bot"):
			require.True(t, strings.HasSuffix(r.URL.Path, "photos/file_1.jpg"))
			_, _ = w.Write([]byte("JPEGBYTES"))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := NewClient("TESTTOKEN", srv.Client())
	c.base = srv.URL

	f, err := c.GetFile(context.Background(), "abc")
	require.NoError(t, err)
	require.Equal(t, "photos/file_1.jpg", f.FilePath)

	data, err := c.DownloadFile(context.Background(), f.FilePath)
	require.NoError(t, err)
	require.Equal(t, "JPEGBYTES", string(data))
}

func TestSendPhotoUploadsMultipart(t *testing.T) {
	var gotField, gotCaption, gotChat string
	var gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasSuffix(r.URL.Path, "/sendPhoto"))
		require.NoError(t, r.ParseMultipartForm(1<<20))
		gotChat = r.FormValue("chat_id")
		gotCaption = r.FormValue("caption")
		for name, fhs := range r.MultipartForm.File {
			gotField = name
			gotFilename = fhs[0].Filename
		}
		w.Header().Set("Content-Type", "application/json")
		res, _ := json.Marshal(Message{MessageID: 7})
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: res})
	}))
	defer srv.Close()
	c := NewClient("TESTTOKEN", srv.Client())
	c.base = srv.URL

	// write a temp image file
	p := filepath.Join(t.TempDir(), "shot.png")
	require.NoError(t, os.WriteFile(p, []byte("PNGDATA"), 0o600))

	m, err := c.SendPhoto(context.Background(), SendPhotoReq{ChatID: -100999, MessageThreadID: 5, Path: p, Caption: "a screenshot"})
	require.NoError(t, err)
	require.Equal(t, int64(7), m.MessageID)
	require.Equal(t, "-100999", gotChat)
	require.Equal(t, "a screenshot", gotCaption)
	require.Equal(t, "photo", gotField)
	require.Equal(t, "shot.png", gotFilename)
}

func TestSendPhotoMissingFile(t *testing.T) {
	c := NewClient("T", nil)
	_, err := c.SendPhoto(context.Background(), SendPhotoReq{ChatID: 1, Path: "/no/such/file.png"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "open")
}

// A photo update parses into Message.Photo (drives the inbound handler).
func TestGetUpdates_ParsesPhotoMessage(t *testing.T) {
	f := newFakeAPI(t)
	f.reply["getUpdates"] = []Update{{UpdateID: 1, Message: &Message{
		MessageID: 3, MessageThreadID: 5, From: &User{ID: 573409113},
		Caption: "look at this", Photo: []PhotoSize{{FileID: "s"}, {FileID: "L", Width: 1280}},
	}}}
	ups, err := f.client().GetUpdates(context.Background(), 0, 1)
	require.NoError(t, err)
	require.Len(t, ups, 1)
	require.Equal(t, "L", ups[0].Message.LargestPhoto())
	require.Equal(t, "look at this", ups[0].Message.Caption)
}
