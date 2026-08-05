// Package client is a thin HTTP client for the arco daemon over its unix
// socket. The CLI and tests use it to drive the same API a Web/Telegram add-on
// would.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/dinhlongviolin1/arco/internal/api"
)

// Client talks to the daemon at a unix socket path.
type Client struct {
	hc *http.Client
}

// New returns a Client dialing the given unix socket.
func New(socket string) *Client {
	return &Client{hc: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("arco: %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Health pings the daemon.
func (c *Client) Health(ctx context.Context) (api.HealthResp, error) {
	var r api.HealthResp
	err := c.do(ctx, http.MethodGet, "/healthz", nil, &r)
	return r, err
}

// Dispatch creates/uses a session and spawns a worker.
func (c *Client) Dispatch(ctx context.Context, req api.DispatchReq) (api.DispatchResp, error) {
	var r api.DispatchResp
	err := c.do(ctx, http.MethodPost, "/v1/dispatch", req, &r)
	return r, err
}

// Workers lists all workers.
func (c *Client) Workers(ctx context.Context) (api.WorkersResp, error) {
	var r api.WorkersResp
	err := c.do(ctx, http.MethodGet, "/v1/workers", nil, &r)
	return r, err
}

// Sessions lists all sessions.
func (c *Client) Sessions(ctx context.Context) (api.SessionsResp, error) {
	var r api.SessionsResp
	err := c.do(ctx, http.MethodGet, "/v1/sessions", nil, &r)
	return r, err
}

// Escalations lists escalations (status defaults to pending server-side).
func (c *Client) Escalations(ctx context.Context) (api.EscalationsResp, error) {
	var r api.EscalationsResp
	err := c.do(ctx, http.MethodGet, "/v1/escalations", nil, &r)
	return r, err
}

// Answer resolves a pending question.
func (c *Client) Answer(ctx context.Context, req api.AnswerReq) error {
	return c.do(ctx, http.MethodPost, "/v1/escalations/answer", req, nil)
}

// Confirm resolves a pending danger-class confirm.
func (c *Client) Confirm(ctx context.Context, req api.ConfirmReq) error {
	return c.do(ctx, http.MethodPost, "/v1/escalations/confirm", req, nil)
}

// PostEvent delivers a herdr-hook state change.
func (c *Client) PostEvent(ctx context.Context, req api.EventReq) (api.EventResp, error) {
	var r api.EventResp
	err := c.do(ctx, http.MethodPost, "/v1/events", req, &r)
	return r, err
}
