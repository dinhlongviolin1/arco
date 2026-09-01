package ledger

import (
	"context"
	"strings"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// maxMessageWindow is the hard server-side ceiling on a durable-history read: no
// caller can request more than this many messages in one query, so the brain
// prompt can never be flooded (the 16 KB context budget is the second guard).
const maxMessageWindow = 500

// messageCols is the shared projection for reading brain_transcript_rows.
const messageCols = `id, session_id, role, content, tainted, created_at`

// AppendMessage records one durable conversation turn. Content is scrubbed at
// this write chokepoint (like event payloads) and the row is mirrored into the
// transcript_fts FTS5 index so SearchMessages can MATCH it.
func (t *txn) AppendMessage(m core.Message) (int64, error) {
	content := m.Content
	if t.scrub != nil {
		content, _ = t.scrub.Scrub(content)
	}
	created := t.now()
	res, err := t.q.ExecContext(context.Background(),
		`INSERT INTO brain_transcript_rows(session_id, role, content, active, compacted, source_events, tainted, created_at)
		 VALUES (?,?,?,1,0,'[]',?,?)`,
		m.SessionID, m.Role, content, boolToInt(m.Tainted), created)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// Mirror the SCRUBBED content into the FTS index (row_id ties it back).
	if _, err := t.q.ExecContext(context.Background(),
		`INSERT INTO transcript_fts(content, session_id, row_id) VALUES (?,?,?)`,
		content, m.SessionID, id); err != nil {
		return 0, err
	}
	return id, nil
}

// RecentMessages returns a session's active messages at or after `since`, bounded
// to the newest `limit` (capped), in chronological order.
func (r *reader) RecentMessages(sessionID string, since time.Time, limit int) ([]core.Message, error) {
	limit = clampLimit(limit)
	// created_at is RFC3339Nano UTC, which is NOT lexically chronological (Go trims
	// trailing fractional zeros / omits the dot on a whole second). So we use the
	// house pattern (see brainrate.go/leases.go): a COARSE whole-second SUPERSET
	// prefilter in SQL (since-1s, secondFmt — always admits, never drops) plus an
	// exact instant filter in Go. Ordering on id (AUTOINCREMENT under the single
	// writer) is the true append order, so the DESC page + reverse is a correct tail.
	pre := since.Add(-time.Second).UTC().Format(secondFmt)
	rows, err := r.q.QueryContext(context.Background(),
		`SELECT `+messageCols+` FROM brain_transcript_rows
		 WHERE session_id=? AND active=1 AND created_at >= ?
		 ORDER BY id DESC LIMIT ?`,
		sessionID, pre, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	// Exact in-Go cutoff (the SQL prefilter is a ~1s-slack superset). The dropped
	// rows are the oldest edge of the page, so this never truncates a full window.
	out := page[:0]
	for _, m := range page {
		if !m.CreatedAt.Before(since) {
			out = append(out, m)
		}
	}
	// Newest-first from the query → reverse to chronological for a readable tail.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// SearchMessages returns a session's active messages whose content matches the
// FTS5 query, newest-first, capped at limit. An empty query returns no rows
// (rather than erroring) so a blank search is a harmless no-op.
func (r *reader) SearchMessages(sessionID, query string, limit int) ([]core.Message, error) {
	limit = clampLimit(limit)
	match := ftsPhrase(query)
	if match == "" {
		return nil, nil
	}
	rows, err := r.q.QueryContext(context.Background(),
		`SELECT `+messageCols+` FROM brain_transcript_rows
		 WHERE session_id=? AND active=1 AND id IN (
		     SELECT row_id FROM transcript_fts WHERE transcript_fts MATCH ? AND session_id = ?
		 )
		 ORDER BY id DESC LIMIT ?`,
		sessionID, match, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func scanMessages(rows rowScanner) ([]core.Message, error) {
	var out []core.Message
	for rows.Next() {
		var (
			m       core.Message
			tainted int
			created string
		)
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &tainted, &created); err != nil {
			return nil, err
		}
		m.Tainted = tainted != 0
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ftsPhrase turns an arbitrary user/brain query into a SAFE FTS5 phrase match:
// FTS5 MATCH has its own syntax (a bare "-" is NOT, "col:" is a column filter,
// unbalanced quotes error), so a raw query like "sk-SECRET" or "a:b" would break
// or misbehave. Wrapping the whole thing in double quotes (with internal quotes
// doubled) makes every character literal — the query is treated as a phrase of
// its tokens. Returns "" for a blank/tokenless query so the caller no-ops.
func ftsPhrase(query string) string {
	q := strings.TrimSpace(query)
	if q == "" {
		return ""
	}
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

func clampLimit(limit int) int {
	if limit <= 0 || limit > maxMessageWindow {
		return maxMessageWindow
	}
	return limit
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// rowScanner is the subset of *sql.Rows scanMessages needs (also lets a test fake).
type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}
