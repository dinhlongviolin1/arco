package features

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

// Searcher searches a session's durable conversation history. core.Reader
// satisfies it (SearchMessages is FTS5-backed and session-scoped).
type Searcher interface {
	SearchMessages(sessionID, query string, limit int) ([]core.Message, error)
}

const historyLimit = 20

// HistoryTool is the on-demand retrieval tool (ADR 0003 D3): the chat brain can
// SEARCH this conversation's durable history for facts not in the recent window,
// instead of everything being force-fed. It is SESSION-BOUND at construction —
// the host (engine.Converse) injects it per turn with the current session id, so
// it can never read another session's messages — and read-only (BrainSafe).
func HistoryTool(s Searcher, sessionID string) feature.Tool {
	return feature.Tool{
		Name:   "history",
		Desc:   `Search THIS conversation's earlier messages for facts not shown in the recent context. Args: {"query":"<words>"}.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"words to search for"}}}`),
		Access: feature.BrainSafe,
		Call: func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Query string `json:"query"`
			}
			// Tolerant of a chatty model's malformed args: a bad/empty object just
			// falls through to the "provide a query" guidance below, not an error.
			_ = json.Unmarshal(args, &a)
			q := strings.TrimSpace(a.Query)
			if q == "" {
				return "provide a search query, e.g. {\"query\":\"the auth bug\"}", nil
			}
			msgs, err := s.SearchMessages(sessionID, q, historyLimit)
			if err != nil {
				return "", err
			}
			if len(msgs) == 0 {
				return "no earlier messages match that.", nil
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "earlier messages matching %q:", q)
			for _, m := range msgs {
				fmt.Fprintf(&sb, "\n  %s: %s", m.Role, truncate(m.Content, 200))
			}
			return sb.String(), nil
		},
	}
}
