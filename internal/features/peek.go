package features

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

// Peeker is the read-only window /peek needs: resolve a bare invocation to the
// single live agent (ScanAgents) and read a pane's recent terminal output
// (PeekAgent). *reconcile.Engine satisfies it.
type Peeker interface {
	ScanAgents(ctx context.Context) ([]core.ScannedAgent, error)
	PeekAgent(ctx context.Context, ref string, lines int) (string, error)
}

// Summarizer turns a prompt into a short natural-language reply (the brain). Used
// only by the operator COMMAND; the brain TOOL returns the raw tail so the chat
// brain summarizes it itself inside the tool-loop (no nested brain call).
type Summarizer func(ctx context.Context, prompt string) (string, error)

const (
	peekLines  = 80
	peekRawCap = 3600 // under Telegram's message limit; the bot scrubs before posting
	peekTailIn = 6000 // how much tail we feed the summarizer / return to the brain
)

// Peek is the /peek capability: read what a herdr session is doing. The operator
// COMMAND returns a brain summary of the pane tail (falling back to the raw tail);
// the brain TOOL returns the raw tail so the chat brain can fold it into its own
// answer. Both are read-only (BrainSafe).
func Peek(p Peeker, summarize Summarizer) feature.Feature {
	return feature.Feature{
		Name: "peek",
		Command: &feature.Command{
			Name: "peek", Usage: "<pane>",
			Help: "summarize what a session is doing (reads its terminal)",
			Run: func(ctx context.Context, in feature.CmdInput) (string, error) {
				ref, err := resolvePane(ctx, p, strings.TrimSpace(in.Arg))
				if err != nil {
					return "", err
				}
				out, err := p.PeekAgent(ctx, ref, peekLines)
				if err != nil {
					return "", err
				}
				if strings.TrimSpace(out) == "" {
					return "peeked " + ref + " — pane is empty / no recent output", nil
				}
				// Ask the brain to summarize; fall back to the raw tail so /peek always
				// gives the operator something. (The bot scrubs the reply before posting.)
				if summarize != nil {
					if s, serr := summarize(ctx, peekPrompt(out)); serr == nil && strings.TrimSpace(s) != "" {
						return "👁 peek " + ref + ":\n" + s, nil
					}
				}
				return "peek " + ref + " (raw tail):\n\n" + truncate(out, peekRawCap), nil
			},
		},
		Tool: &feature.Tool{
			Name:   "peek",
			Desc:   `Read the recent terminal output of a herdr agent pane (read-only). Args: {"pane":"<pane-id>"}. Call scan first to get pane ids.`,
			Schema: json.RawMessage(`{"type":"object","properties":{"pane":{"type":"string","description":"herdr pane id"}}}`),
			Access: feature.BrainSafe,
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Pane string `json:"pane"`
				}
				_ = json.Unmarshal(args, &a) // absent/invalid args → resolve single live below
				ref, err := resolvePane(ctx, p, strings.TrimSpace(a.Pane))
				if err != nil {
					return err.Error(), nil // feed guidance back to the model, not fatal
				}
				out, err := p.PeekAgent(ctx, ref, peekLines)
				if err != nil {
					return "", err
				}
				if strings.TrimSpace(out) == "" {
					return "pane " + ref + " is empty / no recent output", nil
				}
				return truncate(out, peekTailIn), nil
			},
		},
	}
}

// resolvePane returns ref when given; otherwise the single live agent, or an
// error when zero or many are live (the caller must name one).
func resolvePane(ctx context.Context, p Peeker, ref string) (string, error) {
	if ref != "" {
		return ref, nil
	}
	agents, err := p.ScanAgents(ctx)
	if err != nil {
		return "", fmt.Errorf("scan failed: %w", err)
	}
	var live []string
	for _, a := range agents {
		if a.Alive {
			live = append(live, a.Ref)
		}
	}
	if len(live) == 1 {
		return live[0], nil
	}
	return "", fmt.Errorf("name a pane — %d live sessions (see /scan)", len(live))
}

func peekPrompt(tail string) string {
	return "This is the recent terminal output of a coding-agent session. In 2-4 sentences, say what it appears to be working on and its current state. Do not invent details.\n\n" + truncate(tail, peekTailIn)
}
