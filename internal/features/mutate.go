package features

import (
	"context"
	"fmt"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/feature"
)

// Killer terminates a worker. *reconcile.Engine satisfies it via KillWorker.
type Killer interface {
	KillWorker(ctx context.Context, workerID string) error
}

// AdoptFunc registers an untracked herdr agent (by ref/workspace/session id) as a
// monitor-only worker, returning the new worker + session ids. The daemon wraps
// eng.Adopt in this shape so features needn't import the engine's result type.
type AdoptFunc func(ctx context.Context, ref string) (workerID, sessionID string, err error)

// Adopt is the /adopt capability: track existing herdr sessions arco didn't
// launch. With no ref (or "all") it adopts every live UNTRACKED agent; otherwise
// it adopts the one matching the ref. Command-only (mutating; see Kill).
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
// It is a Command ONLY — mutating fleet actions are not brain-callable until the
// native-tool-use host with a grant + escalation path lands (see the BrainAct
// note in feature.go); the read-only chat loop must never terminate a worker.
func Kill(k Killer, l Ledger) feature.Feature {
	return feature.Feature{
		Name: "kill",
		Command: &feature.Command{
			Name: "kill", Usage: "<worker>",
			Help: "terminate a worker (id prefix ok)",
			Run: func(ctx context.Context, in feature.CmdInput) (string, error) {
				frag := strings.TrimSpace(in.Arg)
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
			},
		},
	}
}
