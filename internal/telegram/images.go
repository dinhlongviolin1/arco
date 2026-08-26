package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// Image relay (T6). These are the Bot API primitives for moving images between
// the operator's Telegram and an agent: sendPhoto/sendDocument (outbound, from a
// local file arco holds), and getFile + a downloader (inbound, to pull a photo
// the operator sent into a session's worktree). The agent-side handoff
// convention (how a text-only pane consumes/produces an image) is layered on top
// in the bot, not here.

// maxDownloadBytes caps an inbound file download (Telegram bot downloads are
// limited to 20 MB anyway; this is a hard local ceiling).
const maxDownloadBytes = 25 << 20

// File is the getFile result: a server-side path to fetch via DownloadFile.
type File struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size,omitempty"`
	FilePath string `json:"file_path,omitempty"`
}

// GetFile resolves a file_id to a downloadable file_path.
func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	var f File
	err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f)
	return f, err
}

// DownloadFile fetches the bytes for a getFile file_path from the file endpoint
// (https://api.telegram.org/file/bot<token>/<file_path>). Bounded by
// maxDownloadBytes.
func (c *Client) DownloadFile(ctx context.Context, filePath string) ([]byte, error) {
	url := c.base + "/file/bot" + c.token + "/" + filePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram: download new request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: download: %w", c.scrubToken(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram: download: http %d", resp.StatusCode)
	}
	// Read one byte past the cap so an OVERSIZED file is rejected with an error
	// rather than silently truncated into a corrupt image on disk (review round 5).
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("telegram: download: %w", err)
	}
	if len(b) > maxDownloadBytes {
		return nil, fmt.Errorf("telegram: download exceeds %d bytes — refusing to write a truncated file", maxDownloadBytes)
	}
	return b, nil
}

// SendPhotoReq uploads a local image file as a photo (compressed preview) into a
// chat/topic, with an optional caption.
type SendPhotoReq struct {
	ChatID          int64
	MessageThreadID int64
	Path            string // local file to upload
	Caption         string
}

// SendPhoto uploads Path as a photo via multipart/form-data.
func (c *Client) SendPhoto(ctx context.Context, req SendPhotoReq) (Message, error) {
	return c.uploadFile(ctx, "sendPhoto", "photo", req.ChatID, req.MessageThreadID, req.Path, req.Caption)
}

// SendDocument uploads Path as a document (uncompressed — preserves the exact
// file, e.g. a full-resolution PNG or a non-image artifact).
func (c *Client) SendDocument(ctx context.Context, req SendPhotoReq) (Message, error) {
	return c.uploadFile(ctx, "sendDocument", "document", req.ChatID, req.MessageThreadID, req.Path, req.Caption)
}

// uploadFile is the shared multipart upload for sendPhoto/sendDocument.
func (c *Client) uploadFile(ctx context.Context, method, field string, chatID, threadID int64, path, caption string) (Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return Message{}, fmt.Errorf("telegram: %s: open %s: %w", method, path, err)
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if threadID != 0 {
		_ = mw.WriteField("message_thread_id", strconv.FormatInt(threadID, 10))
	}
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	fw, err := mw.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return Message{}, fmt.Errorf("telegram: %s: form file: %w", method, err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		return Message{}, fmt.Errorf("telegram: %s: copy: %w", method, err)
	}
	if err := mw.Close(); err != nil {
		return Message{}, fmt.Errorf("telegram: %s: close writer: %w", method, err)
	}

	url := c.base + "/bot" + c.token + "/" + method
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return Message{}, fmt.Errorf("telegram: %s: new request: %w", method, err)
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Message{}, fmt.Errorf("telegram: %s: %w", method, c.scrubToken(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env apiResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return Message{}, fmt.Errorf("telegram: %s: decode envelope (http %d): %w", method, resp.StatusCode, err)
	}
	if !env.OK {
		return Message{}, &APIError{Method: method, Code: env.ErrorCode, Description: env.Description}
	}
	var m Message
	if len(env.Result) > 0 {
		_ = json.Unmarshal(env.Result, &m)
	}
	return m, nil
}
