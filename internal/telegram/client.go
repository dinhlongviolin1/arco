// Package telegram implements arco's optional Telegram forum-supergroup
// operator UX: one forum TOPIC per session, a live-edited pinned status card
// per topic, escalation cards with inline-keyboard answer buttons, and an
// inbound long-poll loop that turns a button tap (or a General-topic command)
// into an engine action. It is a self-contained add-on — all Telegram-specific
// code lives here; the reconcile Engine talks to it only through the small
// notify.Sender interface plus the Actions callback interface (bot.go), so the
// core stays free of any Telegram dependency and this package is removable.
//
// Transport is long-poll getUpdates (not a webhook): a self-hosted homelab
// daemon behind NAT/Tailscale must not have to expose a public endpoint.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultBase is the Telegram Bot API root; overridable in tests.
const defaultBase = "https://api.telegram.org"

// Client is a minimal Telegram Bot API HTTP client. Zero value is not usable —
// build one with NewClient. Safe for concurrent use (http.Client is).
type Client struct {
	token string
	base  string
	http  *http.Client
}

// NewClient builds a Bot API client for the given bot token. A nil httpClient
// gets a sane default (30s timeout — long-poll uses its own per-call ctx).
func NewClient(token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{token: token, base: defaultBase, http: httpClient}
}

// Message is the subset of a Telegram message arco cares about.
type Message struct {
	MessageID       int64       `json:"message_id"`
	MessageThreadID int64       `json:"message_thread_id,omitempty"`
	Text            string      `json:"text,omitempty"`
	Caption         string      `json:"caption,omitempty"`
	Chat            Chat        `json:"chat"`
	From            *User       `json:"from,omitempty"`
	Photo           []PhotoSize `json:"photo,omitempty"`    // present for a photo message (multiple sizes)
	Document        *Document   `json:"document,omitempty"` // present for a file/document message
}

// PhotoSize is one size variant of a photo. Telegram sends several; the last is
// the largest.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size,omitempty"`
}

// Document is a file attachment (image sent as a file, or any document).
type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// LargestPhoto returns the highest-resolution photo's file id, or "" if the
// message carries no photo.
func (m *Message) LargestPhoto() string {
	if len(m.Photo) == 0 {
		return ""
	}
	return m.Photo[len(m.Photo)-1].FileID // Telegram orders sizes ascending
}

// Chat identifies a chat/supergroup.
type Chat struct {
	ID int64 `json:"id"`
}

// User identifies a sender.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username,omitempty"`
}

// InlineKeyboardButton is one tappable button; CallbackData is echoed back in a
// CallbackQuery when tapped (Telegram caps it at 64 bytes).
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// InlineKeyboardMarkup is a grid of buttons attached to a message.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// CallbackQuery is delivered when the operator taps an inline button.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// Update is one long-poll update. Exactly one payload field is set.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// apiResponse is the Bot API envelope every method returns.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

// call POSTs a JSON body to /bot<token>/<method> and unmarshals result into out
// (out may be nil to ignore the result). A non-ok envelope is an error carrying
// the Bot API description — the actionable signal (e.g. "chat not found",
// "message thread not found", "message is not modified").
func (c *Client) call(ctx context.Context, method string, body any, out any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return fmt.Errorf("telegram: encode %s: %w", method, err)
	}
	url := c.base + "/bot" + c.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return fmt.Errorf("telegram: new request %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("telegram: %s: read body: %w", method, err)
	}
	var env apiResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram: %s: decode envelope (http %d): %w", method, resp.StatusCode, err)
	}
	if !env.OK {
		return &APIError{Method: method, Code: env.ErrorCode, Description: env.Description}
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("telegram: %s: decode result: %w", method, err)
		}
	}
	return nil
}

// APIError is a non-ok Bot API response. It is comparable via the description
// helpers so callers can treat benign conditions (e.g. an unchanged edit) as
// no-ops rather than failures.
type APIError struct {
	Method      string
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram: %s: api error %d: %s", e.Method, e.Code, e.Description)
}

// IsNotModified reports whether the error is Bot API's "message is not
// modified" — a benign no-op when re-editing a status card to identical text.
func IsNotModified(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code == 400 && strings.Contains(ae.Description, "not modified")
	}
	return false
}

// --- API methods (only what arco uses) ---

// SendMessageReq is the sendMessage payload. MessageThreadID targets a forum
// topic (0 = the General topic / no thread). ReplyMarkup is optional.
type SendMessageReq struct {
	ChatID          int64                 `json:"chat_id"`
	MessageThreadID int64                 `json:"message_thread_id,omitempty"`
	Text            string                `json:"text"`
	ParseMode       string                `json:"parse_mode,omitempty"`
	ReplyMarkup     *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// SendMessage posts a message and returns it (for its message_id).
func (c *Client) SendMessage(ctx context.Context, req SendMessageReq) (Message, error) {
	var m Message
	err := c.call(ctx, "sendMessage", req, &m)
	return m, err
}

// EditMessageTextReq edits an existing message's text (used for the live status
// card — no new notification is produced, so the topic doesn't spam).
type EditMessageTextReq struct {
	ChatID      int64                 `json:"chat_id"`
	MessageID   int64                 `json:"message_id"`
	Text        string                `json:"text"`
	ParseMode   string                `json:"parse_mode,omitempty"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// EditMessageText edits a message in place. IsNotModified(err) is benign.
func (c *Client) EditMessageText(ctx context.Context, req EditMessageTextReq) error {
	return c.call(ctx, "editMessageText", req, nil)
}

// CreateForumTopic creates a named topic in a forum supergroup and returns its
// message_thread_id.
func (c *Client) CreateForumTopic(ctx context.Context, chatID int64, name string) (int64, error) {
	var out struct {
		MessageThreadID int64 `json:"message_thread_id"`
	}
	err := c.call(ctx, "createForumTopic", map[string]any{"chat_id": chatID, "name": name}, &out)
	return out.MessageThreadID, err
}

// CloseForumTopic closes (not deletes) a topic — history is preserved.
func (c *Client) CloseForumTopic(ctx context.Context, chatID, threadID int64) error {
	return c.call(ctx, "closeForumTopic", map[string]any{"chat_id": chatID, "message_thread_id": threadID}, nil)
}

// PinChatMessage pins a message (the per-topic status card) so it stays visible.
func (c *Client) PinChatMessage(ctx context.Context, chatID, messageID int64) error {
	return c.call(ctx, "pinChatMessage",
		map[string]any{"chat_id": chatID, "message_id": messageID, "disable_notification": true}, nil)
}

// AnswerCallbackQuery acknowledges a button tap (clears the client's spinner);
// text (optional) shows as a toast to the operator.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	body := map[string]any{"callback_query_id": id}
	if text != "" {
		body["text"] = text
	}
	return c.call(ctx, "answerCallbackQuery", body, nil)
}

// GetUpdates long-polls for updates after offset, blocking up to timeoutSec on
// the server. The HTTP call uses ctx; give it a deadline a few seconds beyond
// timeoutSec so the long-poll returns naturally rather than being cancelled.
func (c *Client) GetUpdates(ctx context.Context, offset, timeoutSec int) ([]Update, error) {
	var ups []Update
	body := map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message", "callback_query"},
	}
	err := c.call(ctx, "getUpdates", body, &ups)
	return ups, err
}

// GetMe verifies the token and returns the bot's user id/username.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", map[string]any{}, &u)
	return u, err
}
