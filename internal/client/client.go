// Package client is a thin HTTP client for the arco daemon over its unix
// socket. The CLI and tests use it to drive the same API a Web/Telegram add-on
// would.
package client

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/api"
	"github.com/dinhlongviolin1/arco/internal/intakekey"
)

// Client talks to the daemon at a unix socket path.
type Client struct {
	hc           *http.Client
	intakeSecret string // master; derives the per-worker signing key (T3.4)
	intakeKey    string // the worker's own key (from its cred file); wins over derivation
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

// SetIntakeSecret makes the client HMAC-sign POST /v1/events. Set from
// cfg.IntakeSecret so the local `arco hook` bridge keeps working once a shared
// intake secret is configured (else the server's P4 gate 401s the local hook).
// Since T3.4 the wire key is DERIVED per worker from this master.
func (c *Client) SetIntakeSecret(s string) { c.intakeSecret = s }

// SetIntakeKey installs the worker's OWN intake key (the spawn-time
// creds-dir file, see IntakeKeyFromEnv) to sign POST /v1/events verbatim.
// This is how spawned workers sign: they hold their key, never the master.
func (c *Client) SetIntakeKey(k string) { c.intakeKey = k }

// IntakeKeyFromEnv reads the per-worker intake key a spawned worker was handed
// at $CREDENTIALS_DIRECTORY/intake_key; "" when the pointer or file is absent
// (not a spawned worker's hook environment).
func IntakeKeyFromEnv() string {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "intake_key"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// intakeSigningKey picks the key for a /v1/events body: the worker's own key
// file when present, else derived from the master + the event's worker ref
// (T3.4 — the raw master never authenticates on the wire).
func (c *Client) intakeSigningKey(in any) string {
	if c.intakeKey != "" {
		return c.intakeKey
	}
	if ev, ok := in.(api.EventReq); ok {
		return intakekey.Derive(c.intakeSecret, ev.WorkerRef)
	}
	return ""
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	var raw []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		raw, body = b, bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Sign the intake body so the server's P4 HMAC gate accepts a locally-posted
	// event (the hook bridge) once a shared secret is configured. Only /v1/events
	// is verified server-side; signing it over the EXACT marshaled bytes.
	if path == "/v1/events" && raw != nil {
		if key := c.intakeSigningKey(in); key != "" {
			mac := hmac.New(sha256.New, []byte(key))
			mac.Write(raw)
			req.Header.Set("X-Arco-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
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

func (c *Client) KillWorker(ctx context.Context, workerID string) error {
	return c.do(ctx, http.MethodPost, "/v1/workers/"+workerID+"/kill", nil, nil)
}

// Redeliver re-prompts a stranded running worker with its original task
// (operator recovery for a crash-lost initial delivery).
func (c *Client) ImageSend(ctx context.Context, req api.ImageSendReq) (api.ImageSendResp, error) {
	var out api.ImageSendResp
	err := c.do(ctx, "POST", "/v1/image/send", req, &out)
	return out, err
}

func (c *Client) Redeliver(ctx context.Context, workerID string) error {
	return c.do(ctx, http.MethodPost, "/v1/workers/"+workerID+"/redeliver", nil, nil)
}

func (c *Client) CreatePool(ctx context.Context, req api.PoolReq) (api.PoolDTO, error) {
	var r api.PoolDTO
	err := c.do(ctx, http.MethodPost, "/v1/pools", req, &r)
	return r, err
}

func (c *Client) Pools(ctx context.Context) (api.PoolsResp, error) {
	var r api.PoolsResp
	err := c.do(ctx, http.MethodGet, "/v1/pools", nil, &r)
	return r, err
}

// Sessions lists all sessions.
func (c *Client) Sessions(ctx context.Context) (api.SessionsResp, error) {
	var r api.SessionsResp
	err := c.do(ctx, http.MethodGet, "/v1/sessions", nil, &r)
	return r, err
}

// SetSessionMode sets a session's D9 supervision mode (auto|assist|manual).
func (c *Client) SetSessionMode(ctx context.Context, session, mode string) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+session+"/mode", api.ModeReq{Mode: mode}, nil)
}

// Status fetches the one-call fleet snapshot (workers by state, sessions by
// status, pending escalations with age, pool lease usage).
func (c *Client) Status(ctx context.Context) (api.StatusResp, error) {
	var r api.StatusResp
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &r)
	return r, err
}

// Verify moves a completed_candidate worker to completed_verified. expectedRev
// is the rev the caller observed via Diff — the server CASes against it so a
// re-run between review and verify is refused.
func (c *Client) Verify(ctx context.Context, workerID string, expectedRev int64, actor string) error {
	return c.do(ctx, http.MethodPost, "/v1/workers/"+workerID+"/verify",
		api.VerifyReq{ExpectedRev: expectedRev, Actor: actor}, nil)
}

// Diff returns a worker's base→head diff.
func (c *Client) Diff(ctx context.Context, workerID string) (api.DiffResp, error) {
	var r api.DiffResp
	err := c.do(ctx, http.MethodGet, "/v1/workers/"+workerID+"/diff", nil, &r)
	return r, err
}

// EnqueueMerge puts a worker's head on the merge queue (rev7/T3.2).
func (c *Client) EnqueueMerge(ctx context.Context, workerID string) (api.QueueEnqueueResp, error) {
	var r api.QueueEnqueueResp
	err := c.do(ctx, http.MethodPost, "/v1/queue", api.QueueReq{Worker: workerID}, &r)
	return r, err
}

// QueueItems lists the merge queue in FIFO order.
func (c *Client) QueueItems(ctx context.Context) (api.QueueResp, error) {
	var r api.QueueResp
	err := c.do(ctx, http.MethodGet, "/v1/queue", nil, &r)
	return r, err
}

// Autonomy reads the per-class earn-out report (rev7/T3.5).
func (c *Client) Autonomy(ctx context.Context) (api.AutonomyResp, error) {
	var r api.AutonomyResp
	err := c.do(ctx, http.MethodGet, "/v1/autonomy", nil, &r)
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
