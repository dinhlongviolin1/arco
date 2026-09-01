// Package features holds arco's pluggable capabilities as feature.Feature
// values. Each is constructed from a narrow port at the daemon composition root
// and registered once; the registry then binds it to every surface (the Telegram
// command, the brain tool-loop, the MCP server). This is where "add a capability"
// means "add a feature" — one file, one place — instead of editing five layers.
package features

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

// Scanner is the read-only fleet window /scan needs: the live herdr agent
// sessions across every VM, each marked with whether arco already tracks it.
// It speaks core.ScannedAgent (the contract leaf), so this bundle imports only
// core — not the engine. *reconcile.Engine satisfies it directly; tests use a fake.
type Scanner interface {
	ScanAgents(ctx context.Context) ([]core.ScannedAgent, error)
}

// Scan is the /scan capability: list the live herdr agent sessions fleet-wide,
// marking which arco tracks and how to adopt the rest. ONE implementation feeds
// TWO surfaces — the operator command and a BrainSafe tool the chat brain may
// call — so "what's running?" works whether typed as /scan or asked in prose.
func Scan(s Scanner) feature.Feature {
	render := func(ctx context.Context) (string, error) {
		agents, err := s.ScanAgents(ctx)
		if err != nil {
			return "", err
		}
		return renderScan(agents), nil
	}
	return feature.Feature{
		Name: "scan",
		Command: &feature.Command{
			Name: "scan",
			Help: "live herdr agent sessions across the fleet",
			Run:  func(ctx context.Context, _ feature.CmdInput) (string, error) { return render(ctx) },
		},
		Tool: &feature.Tool{
			Name:   "scan",
			Desc:   "List the live herdr agent sessions across the fleet (read-only). Use it to answer what is currently running, including sessions arco did not launch.",
			Access: feature.BrainSafe,
			Call:   func(ctx context.Context, _ json.RawMessage) (string, error) { return render(ctx) },
		},
	}
}

// renderScan formats a scan result for display — the operator-facing text shared
// by the command and the tool. Same structure and marks as the prior hardcoded
// /scan (a long title is elided with a trailing "…").
func renderScan(agents []core.ScannedAgent) string {
	if len(agents) == 0 {
		return "no herdr agent sessions found on the fleet"
	}
	live, done, adoptable := 0, 0, 0
	for _, a := range agents {
		if a.Alive {
			live++
		} else {
			done++
		}
		if a.Alive && !a.Tracked {
			adoptable++
		}
	}
	var sb strings.Builder
	// Count matches what the operator sees in herdr (all panes), with live vs done
	// broken out — a herdr "done" agent is a finished pane, still listed but inert.
	fmt.Fprintf(&sb, "herdr agent sessions (%d — %d live, %d done):\n", len(agents), live, done)
	for _, a := range agents {
		mark := "🆓 untracked"
		switch {
		case !a.Alive:
			mark = "🏁 finished (pane lingering)"
		case a.Tracked:
			mark = "✅ tracked " + short(a.WorkerID)
		}
		fmt.Fprintf(&sb, "\n• %s [%s] on %s — %s\n", a.Kind, a.State, vmLabel(a.VM), mark)
		if a.Title != "" {
			fmt.Fprintf(&sb, "  %s\n", truncate(a.Title, 60))
		}
		if a.Cwd != "" {
			fmt.Fprintf(&sb, "  cwd: %s\n", a.Cwd)
		}
		fmt.Fprintf(&sb, "  pane: %s", a.Ref)
		if a.SessionID != "" {
			fmt.Fprintf(&sb, " · session %s", short(a.SessionID))
		}
		sb.WriteString("\n")
	}
	if adoptable > 0 {
		sb.WriteString("\nadopt with /adopt <pane> (or /adopt all to track every live untracked one)")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// --- small display helpers (local to keep the package decoupled from telegram) ---

// short is the display form of a ULID: its last 8 chars.
func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

// vmLabel is the human name of a VM ("" = the local box).
func vmLabel(vm string) string {
	if vm == "" {
		return "local (this box)"
	}
	return vm
}

// truncate caps s at n bytes (rune-aligned), marking the cut with a trailing "…".
// This is the uniform elision for the features package (scan titles, peek tails,
// worker tasks, session goals). It INTENTIONALLY differs from telegram.truncate's
// older "\n… (truncated)" suffix — the inline ellipsis reads cleaner in a bullet
// and doesn't inject a line break; the earlier /scan review accepted this.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
