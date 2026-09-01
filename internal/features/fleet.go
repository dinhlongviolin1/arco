package features

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

// Ledger is the read-only ledger window the fleet features need. core.Reader
// satisfies it; the daemon passes eng.Store.Reader().
type Ledger interface {
	ListWorkers(f core.WorkerFilter) ([]core.Worker, error)
	ListSessions(f core.SessionFilter) ([]core.Session, error)
	ListEscalations(f core.EscalationFilter) ([]core.Escalation, error)
}

// Workers is the /workers capability: the arco-tracked workers by state. This is
// the LEDGER view (what arco launched/adopted), complementing scan's live-herdr
// view. Read-only.
func Workers(l Ledger) feature.Feature {
	render := func(context.Context) (string, error) { return renderWorkers(l), nil }
	return readFeature("workers", "list active workers by state",
		"List the arco-tracked workers and their states from the ledger (read-only).", render)
}

// Sessions is the /sessions capability: the active issues/sessions arco tracks.
func Sessions(l Ledger) feature.Feature {
	render := func(context.Context) (string, error) { return renderSessions(l), nil }
	return readFeature("sessions", "list active sessions",
		"List the active arco sessions (issues) and their topics from the ledger (read-only).", render)
}

// Status is the /status capability: a one-glance fleet summary (estop + active
// workers by state + pending decisions). Needs the estop state as well as the
// ledger, so it takes a paused func.
func Status(l Ledger, paused func() bool) feature.Feature {
	render := func(context.Context) (string, error) { return renderStatus(l, paused), nil }
	return readFeature("status", "fleet summary (estop, active workers, pending)",
		"Summarize fleet status: emergency-stop state, active worker count by state, and pending decisions (read-only).", render)
}

// VMs is the /vms capability: the attached fleet hosts. Static config (no engine
// call), but exposed as a tool too so the brain can answer "how many VMs?"
// deterministically rather than guessing.
func VMs(lines []string) feature.Feature {
	render := func(context.Context) (string, error) {
		if len(lines) == 0 {
			return "no VMs configured", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "attached VMs (%d):\n", len(lines))
		for _, v := range lines {
			fmt.Fprintf(&b, "• %s\n", v)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	return readFeature("vms", "the attached VMs (fleet hosts)",
		"List the attached VMs / fleet hosts (read-only).", render)
}

// readFeature builds a no-argument read-only feature whose Command and BrainSafe
// Tool share one render — the shape /workers, /sessions, /status all take.
func readFeature(name, help, toolDesc string, render func(context.Context) (string, error)) feature.Feature {
	return feature.Feature{
		Name: name,
		Command: &feature.Command{
			Name: name, Help: help,
			Run: func(ctx context.Context, _ feature.CmdInput) (string, error) { return render(ctx) },
		},
		Tool: &feature.Tool{
			Name: name, Desc: toolDesc, Access: feature.BrainSafe,
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) { return render(ctx) },
		},
	}
}

func renderWorkers(l Ledger) string {
	workers, _ := l.ListWorkers(core.WorkerFilter{})
	var active []core.Worker
	for _, w := range workers {
		if !w.State.Terminal() {
			active = append(active, w)
		}
	}
	if len(active) == 0 {
		return "no active workers"
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	var b strings.Builder
	fmt.Fprintf(&b, "workers (%d active):\n", len(active))
	for _, w := range active {
		vm := "local"
		if w.VM != "" {
			vm = w.VM
		}
		pane := w.AgentRef
		if pane == "" {
			pane = "—"
		}
		fmt.Fprintf(&b, "• %s [%s] vm=%s pane=%s  %s\n", short(w.ID), w.State, vm, pane, truncate(w.Task, 40))
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderSessions(l Ledger) string {
	sessions, _ := l.ListSessions(core.SessionFilter{})
	var b strings.Builder
	n := 0
	for _, s := range sessions {
		if s.Kind == core.SessionKindPool || s.Status == core.SessionDone || s.Status == core.SessionArchived {
			continue
		}
		n++
		label := firstNonEmpty(s.Slug, s.Title, truncate(s.Goal, 40), s.ID)
		topic := "—"
		if s.TGTopicID != nil && *s.TGTopicID != 0 {
			topic = "topic set"
		}
		fmt.Fprintf(&b, "• %s  [%s]  %s\n", label, s.Status, topic)
	}
	if n == 0 {
		return "no active sessions — /dispatch <repo> <task> to start one"
	}
	return fmt.Sprintf("sessions (%d):\n%s", n, strings.TrimRight(b.String(), "\n"))
}

func renderStatus(l Ledger, paused func() bool) string {
	workers, _ := l.ListWorkers(core.WorkerFilter{})
	pending, _ := l.ListEscalations(core.EscalationFilter{Status: "pending"})
	counts := map[string]int{}
	active := 0
	for _, w := range workers {
		if w.State.Terminal() {
			continue
		}
		counts[string(w.State)]++
		active++
	}
	var b strings.Builder
	if paused != nil && paused() {
		b.WriteString("⛔ ESTOP ENGAGED\n")
	} else {
		b.WriteString("▶️ running\n")
	}
	fmt.Fprintf(&b, "active workers: %d\n", active)
	if active > 0 {
		b.WriteString(tally(counts))
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "⏳ pending decisions: %d", len(pending))
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func tally(counts map[string]int) string {
	states := make([]string, 0, len(counts))
	for st := range counts {
		states = append(states, st)
	}
	sort.Strings(states)
	parts := make([]string, 0, len(states))
	for _, st := range states {
		parts = append(parts, fmt.Sprintf("%s×%d", st, counts[st]))
	}
	return strings.Join(parts, "  ")
}
