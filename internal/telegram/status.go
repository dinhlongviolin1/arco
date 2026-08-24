package telegram

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// topicName is the forum-topic title for a session: stable, human-readable, and
// short enough for Telegram's topic-name cap (128 chars).
func topicName(s core.Session) string {
	label := firstNonEmpty(s.Slug, s.Title, truncate(s.Goal, 40), "session "+s.ID)
	return truncate("session: "+label, 120)
}

// renderStatus is the per-session pinned status card: goal, mode, a by-state
// worker tally, and the pending-decision count. Deterministic (states sorted),
// so an identical fleet state re-renders byte-for-byte and the edit is a no-op.
func renderStatus(s core.Session, workers []core.Worker, pending int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📋 %s\n", firstNonEmpty(s.Slug, s.Title, s.ID))
	if s.Goal != "" {
		fmt.Fprintf(&b, "goal: %s\n", truncate(s.Goal, 200))
	}
	fmt.Fprintf(&b, "mode: %s\n", firstNonEmpty(string(s.SupervisionMode), "assist"))

	if len(workers) == 0 {
		b.WriteString("workers: none\n")
	} else {
		counts := map[string]int{}
		for _, w := range workers {
			counts[string(w.State)]++
		}
		states := make([]string, 0, len(counts))
		for st := range counts {
			states = append(states, st)
		}
		sort.Strings(states)
		parts := make([]string, 0, len(states))
		for _, st := range states {
			parts = append(parts, fmt.Sprintf("%s×%d", st, counts[st]))
		}
		fmt.Fprintf(&b, "workers (%d): %s\n", len(workers), strings.Join(parts, "  "))
	}
	if pending > 0 {
		fmt.Fprintf(&b, "⏳ %d decision(s) pending\n", pending)
	}
	return truncate(b.String(), tgMessageCap)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
