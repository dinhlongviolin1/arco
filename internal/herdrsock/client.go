// Package herdrsock is the herdr `events.subscribe` push subscriber (rev7 D1):
// NDJSON over herdr's Unix socket, protocol 17 (verified against `herdr api
// schema --json`). Push is a fusion SIGNAL — faster reaction than the 30s
// polling sweep — never an authority: the ledger reconcile stays king and the
// sweep remains the fallback when frames are dropped or the socket is down.
package herdrsock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// defaultBackoff is the reconnect wait when Client.Backoff is unset.
const defaultBackoff = time.Second

// AgentStatusEvent is one pushed agent-status transition
// (pane.agent_status_changed / pane_agent_status_changed), typed.
type AgentStatusEvent struct {
	PaneID      string
	WorkspaceID string
	Status      string // idle|working|blocked|done|unknown (done is the only terminal)
	Agent       string // agent kind, e.g. "claude"; may be empty
}

// ActivityEvent is a human-activity signal (focus/scroll) — the input the T3.6
// back-off timer will consume. Kind is always the snake_case event kind, no
// matter which envelope spelling delivered it.
type ActivityEvent struct {
	Kind        string // "workspace_focused" | "tab_focused" | "pane_focused" | "pane_scroll_changed"
	PaneID      string
	WorkspaceID string
	TabID       string
}

// Client subscribes to a herdr socket and surfaces typed events. One Client
// per daemon; no global state. All callbacks may be nil (skipped, never a
// panic) and are invoked from the client's read goroutine.
type Client struct {
	SocketPath    string
	Backoff       time.Duration // reconnect backoff (0 ⇒ defaultBackoff)
	OnAgentStatus func(AgentStatusEvent)
	OnActivity    func(ActivityEvent)
	OnResync      func([]core.AgentObs) // full agent.list snapshot after (re)connect
	Logf          func(format string, args ...any)
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Run dials, subscribes, and reads frames until ctx is cancelled, reconnecting
// (after Backoff) on any dial/read error. It blocks; cancel ctx to stop — the
// connection is closed on cancel so a blocked read returns promptly.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.Backoff
	if backoff <= 0 {
		backoff = defaultBackoff
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.session(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// session is one connect→subscribe→resync→read lifetime; it returns on any
// error (the caller redials after backoff).
func (c *Client) session(ctx context.Context) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		c.logf("herdrsock: dial %s: %v", c.SocketPath, err)
		return
	}
	defer conn.Close()
	// Unblock a mid-read shutdown: close the conn as soon as ctx is cancelled.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-watchDone:
		}
	}()

	s := &session{
		c: c, conn: conn,
		panes:       map[string]bool{},
		listIDs:     map[string]bool{},
		pendingSubs: map[string][]string{},
	}
	if err := s.sendSubscribe(true); err != nil {
		c.logf("herdrsock: subscribe: %v", err)
		return
	}
	// Resync on EVERY connect (first and re-): push may have missed transitions
	// while we were not connected, so the full agent.list snapshot re-seeds
	// fusion — and our per-pane subscription set.
	if err := s.sendAgentList(); err != nil {
		c.logf("herdrsock: agent.list: %v", err)
		return
	}
	s.readLoop()
}

// session is the per-connection state. All sends after connect happen from the
// read goroutine, so no write lock is needed.
type session struct {
	c    *Client
	conn net.Conn
	seq  int

	panes       map[string]bool     // pane_ids with a live per-pane subscription
	listIDs     map[string]bool     // outstanding agent.list request ids
	pendingSubs map[string][]string // per-pane subscribe request id → pane_ids it adds
	subID       string              // id of the last FULL subscribe (for the bare-entry retry)
	sentBare    bool                // that subscribe included a pane-less agent-status entry
}

// request is the protocol-17 NDJSON request envelope: id, method, and params
// are all required (request schema: required ["id"], each method ["method","params"]).
type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// frame is any inbound NDJSON line: a response ({"id","result"|"error"}) or an
// async event ({"event","data"}).
type frame struct {
	ID     string          `json:"id"`
	Event  string          `json:"event"`
	Data   json.RawMessage `json:"data"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *session) nextID(kind string) string {
	s.seq++
	return fmt.Sprintf("arco:%s:%d", kind, s.seq)
}

func (s *session) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.conn.Write(append(b, '\n'))
	return err
}

// paneSubs are the pane-scoped subscriptions arco wants for one pane: agent
// status (the D1 signal) and scroll (T3.6 human activity).
func paneSubs(paneID string) []map[string]any {
	return []map[string]any{
		{"type": "pane.agent_status_changed", "pane_id": paneID},
		{"type": "pane.scroll_changed", "pane_id": paneID},
	}
}

// sendSubscribe sends the full events.subscribe covering the D1 signal set:
// the unfiltered focus + pane-lifecycle kinds, plus per-pane agent-status/
// scroll subscriptions for every pane currently known.
//
// Pane-covering strategy — VERIFIED against live herdr protocol 17
// (2026-08-08: `herdr api schema --json` request.$defs.Subscription, plus a
// live socket probe): `pane.agent_status_changed` / `pane.scroll_changed`
// Subscription objects REQUIRE an EXISTING `pane_id`. Omitting it fails the
// WHOLE request with `invalid_request` ("missing field `pane_id`") and an
// EMPTY response id (the request never parses server-side); an unknown pane
// fails it with `pane_not_found`; no wildcard form exists. So panes are
// covered by subscribing the lifecycle kinds unfiltered (pane.created,
// pane.agent_detected, pane.exited, pane.closed) and adding per-pane
// subscriptions as panes appear (agent.list resync + lifecycle events).
// Before any pane is known we still include ONE bare pane.agent_status_changed
// entry — the D1 signal set requests the TYPE even then, and a future protocol
// may accept it; when the server rejects that request we retry once without it
// (see handleError) and rely on the per-pane path.
func (s *session) sendSubscribe(allowBare bool) error {
	subs := []map[string]any{
		{"type": "workspace.focused"},
		{"type": "tab.focused"},
		{"type": "pane.focused"},
		{"type": "pane.created"},
		{"type": "pane.closed"},
		{"type": "pane.exited"},
		{"type": "pane.agent_detected"},
	}
	for pane := range s.panes {
		subs = append(subs, paneSubs(pane)...)
	}
	s.sentBare = false
	if allowBare && len(s.panes) == 0 {
		subs = append(subs, map[string]any{"type": "pane.agent_status_changed"})
		s.sentBare = true
	}
	s.subID = s.nextID("subscribe")
	return s.send(request{ID: s.subID, Method: "events.subscribe",
		Params: map[string]any{"subscriptions": subs}})
}

// sendAgentList requests the full agent snapshot; the response is correlated
// by id in handleFrame and surfaced via OnResync.
func (s *session) sendAgentList() error {
	id := s.nextID("agent-list")
	s.listIDs[id] = true
	return s.send(request{ID: id, Method: "agent.list", Params: map[string]any{}})
}

// subscribePanes adds per-pane subscriptions for panes not yet covered.
func (s *session) subscribePanes(paneIDs []string) {
	var fresh []string
	var subs []map[string]any
	for _, p := range paneIDs {
		if p == "" || s.panes[p] {
			continue
		}
		s.panes[p] = true
		fresh = append(fresh, p)
		subs = append(subs, paneSubs(p)...)
	}
	if len(fresh) == 0 {
		return
	}
	id := s.nextID("panes")
	s.pendingSubs[id] = fresh
	if err := s.send(request{ID: id, Method: "events.subscribe",
		Params: map[string]any{"subscriptions": subs}}); err != nil {
		s.c.logf("herdrsock: subscribe panes: %v", err)
	}
}

// readLoop reads one NDJSON frame per line until disconnect / ctx-cancel close
// (either ends the session; Run redials). Line-delimited framing is what makes
// a malformed frame recoverable: the bad LINE is skipped, the stream stays
// aligned on the next newline.
func (s *session) readLoop() {
	sc := bufio.NewScanner(s.conn)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20) // agent.list snapshots can be large
	for sc.Scan() {
		s.handleFrame(sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		s.c.logf("herdrsock: read: %v", err)
	}
}

// handleFrame processes one inbound frame. Malformed frames are logged and
// skipped — never fatal, never surfaced.
func (s *session) handleFrame(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		s.c.logf("herdrsock: malformed frame skipped: %v", err)
		return
	}
	switch {
	case f.Error != nil:
		s.handleError(f)
	case f.ID != "":
		delete(s.pendingSubs, f.ID) // per-pane subscribe acked
		if s.listIDs[f.ID] {
			delete(s.listIDs, f.ID)
			s.handleAgentList(f.Result)
		}
		// other results (subscription_started acks) need no action
	case f.Event != "":
		s.dispatchEvent(f.Event, f.Data)
	default:
		s.c.logf("herdrsock: unrecognized frame skipped")
	}
}

// handleError routes an error response. Live protocol 17 mangles the id of a
// per-subscription failure ("<id>:sub:<n>:probe") and returns an EMPTY id for
// an unparseable request (the bare pane-less entry), so correlate on the base.
func (s *session) handleError(f frame) {
	baseID := f.ID
	if i := strings.Index(baseID, ":sub:"); i >= 0 {
		baseID = baseID[:i]
	}
	if s.sentBare && (baseID == s.subID || f.ID == "") {
		// The optimistic pane-less agent-status entry was rejected (expected on
		// live protocol 17); retry the full subscribe without it.
		s.c.logf("herdrsock: pane-less agent-status subscription rejected (%s: %s); resubscribing per-pane only",
			f.Error.Code, f.Error.Message)
		if err := s.sendSubscribe(false); err != nil {
			s.c.logf("herdrsock: resubscribe: %v", err)
		}
		return
	}
	if panes, ok := s.pendingSubs[baseID]; ok {
		// A per-pane subscribe failed (e.g. the pane vanished first — the whole
		// request is rejected): forget those panes so a later signal re-adds them.
		delete(s.pendingSubs, baseID)
		for _, p := range panes {
			delete(s.panes, p)
		}
		s.c.logf("herdrsock: per-pane subscribe failed (%s: %s); dropped %v", f.Error.Code, f.Error.Message, panes)
		return
	}
	s.c.logf("herdrsock: error response id=%q %s: %s", f.ID, f.Error.Code, f.Error.Message)
}

// dispatchEvent types one async event and invokes the matching callback.
// Two envelope families exist (verified against the protocol-17 schema):
// EventEnvelope kinds are snake_case ("pane_agent_status_changed") and
// SubscriptionEventEnvelope kinds are dotted ("pane.agent_status_changed") —
// both spellings are accepted by normalizing to snake_case. Kinds arco does
// not consume are silently dropped.
func (s *session) dispatchEvent(kind string, data json.RawMessage) {
	kind = strings.ReplaceAll(kind, ".", "_")
	switch kind {
	case "pane_agent_status_changed":
		var d struct {
			PaneID      string `json:"pane_id"`
			WorkspaceID string `json:"workspace_id"`
			AgentStatus string `json:"agent_status"`
			Agent       string `json:"agent"`
		}
		if err := json.Unmarshal(data, &d); err != nil || d.PaneID == "" || d.AgentStatus == "" {
			s.c.logf("herdrsock: bad agent-status frame skipped")
			return
		}
		if cb := s.c.OnAgentStatus; cb != nil {
			cb(AgentStatusEvent{PaneID: d.PaneID, WorkspaceID: d.WorkspaceID, Status: d.AgentStatus, Agent: d.Agent})
		}
	case "workspace_focused", "tab_focused", "pane_focused", "pane_scroll_changed":
		var d struct {
			PaneID      string `json:"pane_id"`
			WorkspaceID string `json:"workspace_id"`
			TabID       string `json:"tab_id"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			s.c.logf("herdrsock: bad %s frame skipped", kind)
			return
		}
		if cb := s.c.OnActivity; cb != nil {
			cb(ActivityEvent{Kind: kind, PaneID: d.PaneID, WorkspaceID: d.WorkspaceID, TabID: d.TabID})
		}
	case "pane_created":
		var d struct {
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		}
		if err := json.Unmarshal(data, &d); err != nil || d.Pane.PaneID == "" {
			return
		}
		s.subscribePanes([]string{d.Pane.PaneID})
	case "pane_agent_detected":
		var d struct {
			PaneID string `json:"pane_id"`
		}
		if err := json.Unmarshal(data, &d); err != nil || d.PaneID == "" {
			return
		}
		s.subscribePanes([]string{d.PaneID})
	case "pane_exited", "pane_closed":
		var d struct {
			PaneID string `json:"pane_id"`
		}
		if err := json.Unmarshal(data, &d); err == nil {
			delete(s.panes, d.PaneID)
		}
	}
}

// handleAgentList parses an agent.list result ({"type":"agent_list","agents":
// [...]}) into core.AgentObs with the SAME mapping the polling client uses
// (internal/vm/local.go ListAgents): pane_id→Ref, workspace_id→Workspace,
// terminal_id→BootID, agent_status→State, Alive = status != "done" (done is
// the only terminal agent_status — docs/herdr-contract.md). The snapshot also
// seeds per-pane subscriptions for every listed pane.
func (s *session) handleAgentList(result json.RawMessage) {
	var res struct {
		Type   string `json:"type"`
		Agents []struct {
			Agent       string `json:"agent"`
			AgentStatus string `json:"agent_status"`
			PaneID      string `json:"pane_id"`
			WorkspaceID string `json:"workspace_id"`
			TerminalID  string `json:"terminal_id"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(result, &res); err != nil || res.Type != "agent_list" {
		s.c.logf("herdrsock: bad agent.list response skipped")
		return
	}
	obs := make([]core.AgentObs, 0, len(res.Agents))
	panes := make([]string, 0, len(res.Agents))
	for _, a := range res.Agents {
		obs = append(obs, core.AgentObs{
			Ref: a.PaneID, Workspace: a.WorkspaceID, BootID: a.TerminalID,
			State: a.AgentStatus, Alive: a.AgentStatus != "done",
		})
		panes = append(panes, a.PaneID)
	}
	if cb := s.c.OnResync; cb != nil {
		cb(obs)
	}
	s.subscribePanes(panes)
}
