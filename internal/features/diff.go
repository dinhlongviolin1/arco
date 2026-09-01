package features

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

// WorkerDiffer produces a worker's base→head diff. *reconcile.Engine satisfies it.
type WorkerDiffer interface {
	WorkerDiff(ctx context.Context, workerID string) (core.Diff, error)
}

// diffToolCap bounds the diff handed to the chat brain (model-context headroom).
// The COMMAND is not capped here — the command chokepoint scrubs THEN truncates,
// so redaction always sees the whole patch before any cut.
const diffToolCap = 6000

// Diff is the /diff capability: a worker's redacted unified diff. The command
// posts it to the operator (scrubbed + capped at the command chokepoint); the
// BrainSafe tool returns it for the chat brain (scrubbed by Converse, in and
// out). Read-only.
func Diff(d WorkerDiffer, l Ledger) feature.Feature {
	build := func(ctx context.Context, fragment string) (string, error) {
		w, err := resolveWorkerByID(l, strings.TrimSpace(fragment))
		if err != nil {
			return "", err
		}
		dd, err := d.WorkerDiff(ctx, w.ID)
		if err != nil {
			return "", fmt.Errorf("diff error: %w", err)
		}
		patch := dd.Patch
		if dd.Truncated {
			patch += "\n… (diff truncated by arco)"
		}
		if strings.TrimSpace(patch) == "" {
			patch = "(no diff — base == head)"
		}
		return "diff — " + short(w.ID) + "\n\n" + patch, nil
	}
	return feature.Feature{
		Name: "diff",
		Command: &feature.Command{
			Name: "diff", Usage: "<worker>",
			Help: "show a worker's redacted diff (id prefix ok)",
			Run: func(ctx context.Context, in feature.CmdInput) (string, error) {
				if strings.TrimSpace(in.Arg) == "" {
					return "", fmt.Errorf("give a worker id (a prefix is fine)")
				}
				return build(ctx, in.Arg) // full patch; chokepoint scrubs then truncates
			},
		},
		Tool: &feature.Tool{
			Name:   "diff",
			Desc:   `Show a worker's git diff (read-only). Args: {"worker":"<id-or-prefix>"}. Call workers first to get ids.`,
			Schema: json.RawMessage(`{"type":"object","properties":{"worker":{"type":"string","description":"worker id or a distinctive prefix"}}}`),
			Access: feature.BrainSafe,
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Worker string `json:"worker"`
				}
				_ = json.Unmarshal(args, &a)
				if strings.TrimSpace(a.Worker) == "" {
					return "provide a worker id (call the workers tool first)", nil
				}
				out, err := build(ctx, a.Worker)
				if err != nil {
					return err.Error(), nil // feed guidance back to the model, not fatal
				}
				// Cap for model-context headroom. NOTE the asymmetry with the command
				// path (which scrubs-then-truncates at the chokepoint): here we truncate
				// BEFORE Engine.Converse scrubs the rendered prompt, so a secret sitting
				// exactly on the diffToolCap boundary could be split into a fragment the
				// scrub misses. Accepted: it's a partial (not the intact secret), the only
				// reader is the third-party model, the cap is generous, and Converse
				// re-scrubs the final answer before it reaches the operator.
				return truncate(out, diffToolCap), nil
			},
		},
	}
}

// resolveWorkerByID finds the single worker whose id contains the fragment
// (case-insensitive) — the same handle /workers shows and /kill accepts.
func resolveWorkerByID(l Ledger, fragment string) (core.Worker, error) {
	up := strings.ToUpper(fragment)
	workers, err := l.ListWorkers(core.WorkerFilter{})
	if err != nil {
		return core.Worker{}, fmt.Errorf("lookup failed: %w", err)
	}
	var matches []core.Worker
	for _, w := range workers {
		if strings.Contains(strings.ToUpper(w.ID), up) {
			matches = append(matches, w)
		}
	}
	switch len(matches) {
	case 0:
		return core.Worker{}, fmt.Errorf("no worker matches %q", fragment)
	case 1:
		return matches[0], nil
	default:
		return core.Worker{}, fmt.Errorf("%q matches %d workers — use more of the id", fragment, len(matches))
	}
}
