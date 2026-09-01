package features

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/feature"
)

// Killer terminates a worker. *reconcile.Engine satisfies it via KillWorker.
type Killer interface {
	KillWorker(ctx context.Context, workerID string) error
}

// DispatchFunc starts a worker on repo to do task, returning the new worker +
// session ids. The daemon wraps eng.Spawn (new-issue path); the session's Telegram
// topic opens on its first card, as usual.
type DispatchFunc func(ctx context.Context, repo, task string) (workerID, sessionID string, err error)

// Dispatch lets the brain PROPOSE starting a new worker (a new issue + its own
// topic). Tool-only + BrainAct — the /dispatch operator command keeps its richer
// topic-opening path in the telegram layer; this is the brain's gated proposal.
func Dispatch(dispatch DispatchFunc) feature.Feature {
	return feature.Feature{
		Name: "dispatch",
		Tool: &feature.Tool{
			Name:   "dispatch",
			Desc:   `Start a new worker on a repo to do a task (opens a new issue + topic). Args: {"repo":"<path>","task":"<what to do>"}.`,
			Schema: json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"},"task":{"type":"string"}},"required":["repo","task"]}`),
			Access: feature.BrainAct,
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Repo string `json:"repo"`
					Task string `json:"task"`
				}
				_ = json.Unmarshal(args, &a)
				repo, task := strings.TrimSpace(a.Repo), strings.TrimSpace(a.Task)
				if repo == "" || task == "" {
					return `provide {"repo":"<path>","task":"<what to do>"}`, nil
				}
				wid, sid, err := dispatch(ctx, repo, task)
				if err != nil {
					return "dispatch failed: " + err.Error(), nil
				}
				return fmt.Sprintf("🚀 started worker %s (session %s) on %s", short(wid), short(sid), repo), nil
			},
		},
	}
}

// AdoptFunc registers an untracked herdr agent (by ref/workspace/session id) as a
// monitor-only worker, returning the new worker + session ids. The daemon wraps
// eng.Adopt in this shape so features needn't import the engine's result type.
type AdoptFunc func(ctx context.Context, ref string) (workerID, sessionID string, err error)

// Adopt is the /adopt capability: track existing herdr sessions arco didn't
// launch. With no ref (or "all") it adopts every live UNTRACKED agent; otherwise
// it adopts the one matching the ref. The BrainAct tool lets the brain PROPOSE an
// adopt — gated by the operator's confirm/off policy in the tool-loop.
func Adopt(scan Scanner, adopt AdoptFunc) feature.Feature {
	return feature.Feature{
		Name: "adopt",
		Command: &feature.Command{
			Name: "adopt", Usage: "[ref]",
			Help: "track an existing herdr session (no ref → all untracked)",
			Run: func(ctx context.Context, in feature.CmdInput) (string, error) {
				arg := strings.TrimSpace(in.Arg)
				if arg == "" || strings.EqualFold(arg, "all") {
					return adoptAll(ctx, scan, adopt)
				}
				wid, sid, err := adopt(ctx, arg)
				if err != nil {
					return "", fmt.Errorf("adopt failed: %w", err)
				}
				return fmt.Sprintf("👁 adopted %s as worker %s (session %s)\nmonitor-only (manual mode): arco tracks liveness + relays, but didn't launch it so it can't enforce grants.", arg, short(wid), short(sid)), nil
			},
		},
		Tool: &feature.Tool{
			Name:   "adopt",
			Desc:   `Track an untracked herdr session as a monitor-only worker. Args: {"ref":"<pane>"}. Call scan first for pane refs.`,
			Schema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string"}}}`),
			Access: feature.BrainAct,
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Ref string `json:"ref"`
				}
				_ = json.Unmarshal(args, &a)
				ref := strings.TrimSpace(a.Ref)
				if ref == "" {
					return "provide a pane ref, e.g. {\"ref\":\"w1:p1\"} (call scan first)", nil
				}
				wid, sid, err := adopt(ctx, ref)
				if err != nil {
					return "adopt failed: " + err.Error(), nil // guidance, not fatal
				}
				return fmt.Sprintf("👁 adopted %s as worker %s (session %s, monitor-only)", ref, short(wid), short(sid)), nil
			},
		},
	}
}

// adoptAll adopts every live untracked agent (finished/done panes are skipped —
// nothing to monitor), reporting per-ref outcomes.
func adoptAll(ctx context.Context, scan Scanner, adopt AdoptFunc) (string, error) {
	agents, err := scan.ScanAgents(ctx)
	if err != nil {
		return "", fmt.Errorf("adopt failed (scan): %w", err)
	}
	var refs []string
	for _, a := range agents {
		if a.Alive && !a.Tracked {
			refs = append(refs, a.Ref)
		}
	}
	if len(refs) == 0 {
		return "nothing to adopt — every live agent is already tracked (/scan to see)", nil
	}
	var sb strings.Builder
	for _, ref := range refs {
		wid, sid, err := adopt(ctx, ref)
		if err != nil {
			fmt.Fprintf(&sb, "• %s — skipped: %s\n", ref, err.Error())
			continue
		}
		fmt.Fprintf(&sb, "• %s — 👁 adopted as worker %s (session %s, monitor-only)\n", ref, short(wid), short(sid))
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// Kill is the /kill capability: terminate a worker resolved by an id fragment.
// The BrainAct tool lets the brain PROPOSE a kill; whether it runs is the
// operator's per-feature policy (default confirm — a Telegram ✅), applied by the
// tool-loop. The read-only loop never kills without that gate.
func Kill(k Killer, l Ledger) feature.Feature {
	// do resolves a worker by fragment and terminates it — the pure executor shared
	// by the operator command and the brain tool (the tool is BrainAct, so the loop
	// applies the confirm/auto/off policy around this; do itself just executes).
	do := func(ctx context.Context, fragment string) (string, error) {
		frag := strings.TrimSpace(fragment)
		if frag == "" {
			return "", fmt.Errorf("give a worker id (a prefix is fine)")
		}
		w, err := resolveWorkerByID(l, frag)
		if err != nil {
			return "", err
		}
		if err := k.KillWorker(ctx, w.ID); err != nil {
			return "", fmt.Errorf("kill failed: %w", err)
		}
		return "🛑 killed worker " + short(w.ID), nil
	}
	return feature.Feature{
		Name: "kill",
		Command: &feature.Command{
			Name: "kill", Usage: "<worker>",
			Help: "terminate a worker (id prefix ok)",
			Run:  func(ctx context.Context, in feature.CmdInput) (string, error) { return do(ctx, in.Arg) },
		},
		Tool: &feature.Tool{
			Name:   "kill",
			Desc:   `Terminate a running worker by id or id-prefix. Args: {"worker":"<id>"}. Call workers first for ids.`,
			Schema: json.RawMessage(`{"type":"object","properties":{"worker":{"type":"string"}}}`),
			Access: feature.BrainAct,
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Worker string `json:"worker"`
				}
				_ = json.Unmarshal(args, &a)
				out, err := do(ctx, a.Worker)
				if err != nil {
					return err.Error(), nil // resolve/kill failure → guidance back to the model, not fatal
				}
				return out, nil
			},
		},
	}
}
