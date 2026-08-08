package herdrsock

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// subscribeRequests decodes every events.subscribe request the connection has
// seen so far, in order.
func subscribeRequests(c *fakeConn) []map[string]any {
	var out []map[string]any
	for _, l := range c.requests() {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		if m["method"] == "events.subscribe" {
			out = append(out, m)
		}
	}
	return out
}

// hasSub reports whether a subscribe request carries a subscription of the
// given type and pane_id ("" = a pane-less entry).
func hasSub(req map[string]any, typ, paneID string) bool {
	params, _ := req["params"].(map[string]any)
	subs, _ := params["subscriptions"].([]any)
	for _, s := range subs {
		m, _ := s.(map[string]any)
		if m == nil || m["type"] != typ {
			continue
		}
		p, _ := m["pane_id"].(string)
		if p == paneID {
			return true
		}
	}
	return false
}

// Live herdr protocol 17 (verified 2026-08-08) rejects a pane-less
// pane.agent_status_changed subscription: the WHOLE request fails with
// invalid_request and an EMPTY response id (it never parses server-side). The
// client must fall back — re-subscribe without the bare entry — and then cover
// panes per-pane as the agent.list resync reveals them.
func TestSubscribe_BareRejectionFallsBackToPerPane(t *testing.T) {
	f, sock := newFakeHerdr(t)
	rec := &recorder{}
	startClient(t, sock, rec)

	conn := f.waitConn(t)
	first := conn.waitRequest(t, "events.subscribe")
	require.True(t, hasSub(first, "pane.agent_status_changed", ""),
		"before any pane is known the initial subscribe carries the pane-less agent-status entry")

	// The live server's rejection shape: whole request refused, empty id.
	conn.send(t, `{"id":"","error":{"code":"invalid_request","message":"invalid request: missing field pane_id"}}`)

	waitFor(t, "degraded resubscribe without the bare entry", func() bool {
		subs := subscribeRequests(conn)
		return len(subs) >= 2 && !hasSub(subs[len(subs)-1], "pane.agent_status_changed", "")
	})

	// The resync snapshot then seeds a per-pane agent-status subscription.
	list := conn.waitRequest(t, "agent.list")
	id, _ := list["id"].(string)
	require.NotEmpty(t, id)
	conn.send(t, `{"id":"`+id+`","result":{"type":"agent_list","agents":[`+
		`{"agent":"claude","agent_status":"working","pane_id":"wB:p1","workspace_id":"wB","terminal_id":"term_1"}]}}`)

	waitFor(t, "per-pane agent-status subscription", func() bool {
		for _, r := range subscribeRequests(conn) {
			if hasSub(r, "pane.agent_status_changed", "wB:p1") {
				return true
			}
		}
		return false
	})
}
