// Guideline tests for T2.1 (rev7 D1): the herdr `events.subscribe` NDJSON
// Unix-socket client that replaces polling as the primary signal input.
// These tests run a FAKE herdr socket server (an in-test unix listener speaking
// newline-delimited JSON with protocol-17 shapes) and assert the client's
// behavior: subscribe on connect, surface typed events, ignore malformed
// frames, reconnect with `agent.list` resync, and never surface event kinds
// that were not subscribed.
package herdrsock

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// fakeHerdr is a minimal NDJSON unix-socket server: each accepted connection
// records the client's request lines and plays scripted frames back.
type fakeHerdr struct {
	t        *testing.T
	ln       net.Listener
	mu       sync.Mutex
	conns    []*fakeConn
	acceptCh chan *fakeConn
}

type fakeConn struct {
	net.Conn
	mu    sync.Mutex
	lines []string
}

func (c *fakeConn) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// send writes one NDJSON frame to the client.
func (c *fakeConn) send(t *testing.T, frame string) {
	t.Helper()
	_, err := c.Conn.Write([]byte(frame + "\n"))
	require.NoError(t, err)
}

func newFakeHerdr(t *testing.T) (*fakeHerdr, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "herdr.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	f := &fakeHerdr{t: t, ln: ln, acceptCh: make(chan *fakeConn, 4)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fc := &fakeConn{Conn: c}
			go func() {
				sc := bufio.NewScanner(fc.Conn)
				for sc.Scan() {
					fc.mu.Lock()
					fc.lines = append(fc.lines, sc.Text())
					fc.mu.Unlock()
				}
			}()
			f.mu.Lock()
			f.conns = append(f.conns, fc)
			f.mu.Unlock()
			f.acceptCh <- fc
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f, sock
}

// waitConn waits for the next client connection.
func (f *fakeHerdr) waitConn(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-f.acceptCh:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("client never connected")
		return nil
	}
}

// waitRequest polls until the connection has seen a request whose "method"
// equals method, and returns its decoded JSON object.
func (c *fakeConn) waitRequest(t *testing.T, method string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range c.requests() {
			var m map[string]any
			if json.Unmarshal([]byte(l), &m) != nil {
				continue
			}
			if m["method"] == method {
				return m
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("client never sent a %q request; saw: %v", method, c.requests())
	return nil
}

// recorder collects the client's surfaced updates thread-safely.
type recorder struct {
	mu       sync.Mutex
	statuses []AgentStatusEvent
	activity []ActivityEvent
	resyncs  [][]core.AgentObs
}

func (r *recorder) onStatus(ev AgentStatusEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, ev)
}
func (r *recorder) onActivity(ev ActivityEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activity = append(r.activity, ev)
}
func (r *recorder) onResync(agents []core.AgentObs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resyncs = append(r.resyncs, agents)
}
func (r *recorder) statusList() []AgentStatusEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AgentStatusEvent(nil), r.statuses...)
}
func (r *recorder) activityList() []ActivityEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ActivityEvent(nil), r.activity...)
}
func (r *recorder) resyncList() [][]core.AgentObs {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]core.AgentObs(nil), r.resyncs...)
}

// startClient runs the subscriber in a goroutine with a short reconnect
// backoff and stops it at test end.
func startClient(t *testing.T, sock string, rec *recorder) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		SocketPath:    sock,
		Backoff:       20 * time.Millisecond,
		OnAgentStatus: rec.onStatus,
		OnActivity:    rec.onActivity,
		OnResync:      rec.onResync,
	}
	go c.Run(ctx)
	t.Cleanup(cancel)
	return cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// On connect the client must send an events.subscribe request whose
// subscription set covers the signal kinds arco consumes (agent status +
// human-activity focus/scroll), then surface a pushed
// pane.agent_status_changed as a typed AgentStatusEvent.
func TestSubscribe_AgentStatusEventSurfaced(t *testing.T) {
	f, sock := newFakeHerdr(t)
	rec := &recorder{}
	startClient(t, sock, rec)

	conn := f.waitConn(t)
	req := conn.waitRequest(t, "events.subscribe")
	params, _ := req["params"].(map[string]any)
	require.NotNil(t, params, "events.subscribe carries params")
	subs, _ := params["subscriptions"].([]any)
	require.NotEmpty(t, subs, "subscriptions must not be empty")
	types := map[string]bool{}
	for _, s := range subs {
		m, _ := s.(map[string]any)
		require.NotNil(t, m)
		typ, _ := m["type"].(string)
		require.NotEmpty(t, typ, "every subscription names its type")
		types[typ] = true
	}
	// The D1 signal set: agent status transitions plus the human-activity
	// signals T3.6 will consume. (Exact per-pane filtering is the client's
	// business; the TYPES must be requested.)
	for _, want := range []string{"pane.agent_status_changed", "pane.focused", "workspace.focused", "tab.focused"} {
		require.True(t, types[want], "subscription set must include %s (got %v)", want, types)
	}

	conn.send(t, `{"event":"pane.agent_status_changed","data":{"pane_id":"wB:p1","workspace_id":"wB","agent_status":"blocked","agent":"claude"}}`)
	waitFor(t, "agent status event", func() bool { return len(rec.statusList()) == 1 })
	ev := rec.statusList()[0]
	require.Equal(t, "wB:p1", ev.PaneID)
	require.Equal(t, "wB", ev.WorkspaceID)
	require.Equal(t, "blocked", ev.Status)
}

// Focus/scroll events surface as ActivityEvents (the human-activity signal for
// T3.6) carrying the event kind and the id it concerns.
func TestSubscribe_FocusEventSurfacesAsActivity(t *testing.T) {
	f, sock := newFakeHerdr(t)
	rec := &recorder{}
	startClient(t, sock, rec)

	conn := f.waitConn(t)
	conn.waitRequest(t, "events.subscribe")

	conn.send(t, `{"event":"workspace_focused","data":{"type":"workspace_focused","workspace_id":"wB"}}`)
	conn.send(t, `{"event":"pane_focused","data":{"type":"pane_focused","pane_id":"wB:p1"}}`)

	waitFor(t, "two activity events", func() bool { return len(rec.activityList()) == 2 })
	acts := rec.activityList()
	require.Equal(t, "workspace_focused", acts[0].Kind)
	require.Equal(t, "wB", acts[0].WorkspaceID)
	require.Equal(t, "pane_focused", acts[1].Kind)
	require.Equal(t, "wB:p1", acts[1].PaneID)
	require.Empty(t, rec.statusList(), "focus events are activity, not agent status")
}

// Malformed frames (not JSON, wrong shape, empty) are IGNORED — the connection
// survives and later valid frames still surface.
func TestMalformedFramesIgnored(t *testing.T) {
	f, sock := newFakeHerdr(t)
	rec := &recorder{}
	startClient(t, sock, rec)

	conn := f.waitConn(t)
	conn.waitRequest(t, "events.subscribe")

	conn.send(t, `this is not json`)
	conn.send(t, `{"event":123,"data":"nope"}`)
	conn.send(t, `{}`)
	conn.send(t, `{"event":"pane.agent_status_changed","data":{"pane_id":"wB:p9","workspace_id":"wB","agent_status":"working"}}`)

	waitFor(t, "the valid frame after garbage", func() bool { return len(rec.statusList()) == 1 })
	require.Equal(t, "wB:p9", rec.statusList()[0].PaneID)
	require.Equal(t, "working", rec.statusList()[0].Status)
}

// Event kinds the client did not subscribe to (or cannot type) must NOT
// surface through any callback — filter correctness, no panic.
func TestUnsubscribedKindsNotSurfaced(t *testing.T) {
	f, sock := newFakeHerdr(t)
	rec := &recorder{}
	startClient(t, sock, rec)

	conn := f.waitConn(t)
	conn.waitRequest(t, "events.subscribe")

	conn.send(t, `{"event":"layout_updated","data":{"type":"layout_updated"}}`)
	conn.send(t, `{"event":"pane.output_matched","data":{"pane_id":"wB:p1","matched_line":"x"}}`)
	// A valid status frame AFTER the unsubscribed ones proves they were skipped
	// (not fatal) and gives the waiter something deterministic.
	conn.send(t, `{"event":"pane.agent_status_changed","data":{"pane_id":"wB:p1","workspace_id":"wB","agent_status":"idle"}}`)

	waitFor(t, "the status frame", func() bool { return len(rec.statusList()) == 1 })
	require.Empty(t, rec.activityList(), "unsubscribed kinds must not surface as activity")
}

// When the connection drops the client reconnects (after backoff),
// re-subscribes, and issues an `agent.list` RESYNC — push can have missed
// transitions while disconnected, so the full snapshot re-seeds fusion. The
// parsed agents surface through OnResync as core.AgentObs (workspace_id →
// Workspace, pane_id → Ref, terminal_id → BootID, done ⇒ not Alive — the
// same mapping the polling client uses).
func TestReconnect_ResubscribesAndResyncs(t *testing.T) {
	f, sock := newFakeHerdr(t)
	rec := &recorder{}
	startClient(t, sock, rec)

	conn1 := f.waitConn(t)
	conn1.waitRequest(t, "events.subscribe")
	require.NoError(t, conn1.Conn.Close()) // the server drops the connection

	conn2 := f.waitConn(t) // the client must come back
	req := conn2.waitRequest(t, "events.subscribe")
	require.NotNil(t, req, "reconnect must re-subscribe")

	listReq := conn2.waitRequest(t, "agent.list")
	id, _ := listReq["id"].(string)
	require.NotEmpty(t, id, "agent.list request carries an id to correlate the response")
	conn2.send(t, `{"id":"`+id+`","result":{"type":"agent_list","agents":[`+
		`{"agent":"claude","agent_status":"working","pane_id":"wB:p1","workspace_id":"wB","terminal_id":"term_1"},`+
		`{"agent":"claude","agent_status":"done","pane_id":"wC:p2","workspace_id":"wC","terminal_id":"term_2"}]}}`)

	waitFor(t, "resync snapshot", func() bool { return len(rec.resyncList()) >= 1 })
	obs := rec.resyncList()[0]
	require.Len(t, obs, 2)
	require.Equal(t, "wB", obs[0].Workspace)
	require.Equal(t, "wB:p1", obs[0].Ref)
	require.Equal(t, "term_1", obs[0].BootID)
	require.True(t, obs[0].Alive, "working ⇒ alive")
	require.Equal(t, "done", obs[1].State)
	require.False(t, obs[1].Alive, "done is the only terminal agent_status")
}

// Run returns promptly when the context is canceled (clean daemon shutdown),
// even while mid-reconnect-backoff against a dead socket.
func TestRun_StopsOnContextCancel(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nobody-home.sock") // nothing listening
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{SocketPath: sock, Backoff: 10 * time.Millisecond,
		OnAgentStatus: rec.onStatus, OnActivity: rec.onActivity, OnResync: rec.onResync}
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond) // let it spin through a few failed dials
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
