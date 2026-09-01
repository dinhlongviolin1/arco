package reconcile

import (
	"context"
	"fmt"

	"github.com/dinhlongviolin1/arco/internal/brain"
	"github.com/dinhlongviolin1/arco/internal/feature"
	"github.com/dinhlongviolin1/arco/internal/features"
	"github.com/dinhlongviolin1/arco/internal/toolloop"
)

// Converse runs an AGENTIC conversational turn: the operator's message plus a
// set of read-only (BrainSafe) tools the brain may call to gather facts, via the
// text-protocol tool-loop (clavis has no native tool-use). It replaces the
// one-shot BrainReply for chat — instead of pre-stuffing fleet facts, the brain
// fetches them itself by calling e.g. the scan tool.
//
// Security/cost, enforced HERE (the loop is pure logic and does none of it):
//   - Redaction runs on the ENTIRE rendered prompt each round — the prompt now
//     carries verbatim tool output (paths, titles, terminal tails), so scrubbing
//     only the user message would leak secrets to the third-party model.
//   - Every round is metered like any other brain call, and the loop's hard
//     round cap bounds calls-per-turn.
//   - Only BrainSafe tools execute (the loop refuses the rest); estop/Paused is
//     NOT a bar here because this path is read-only — the operator can still ask
//     "what's running?" during an emergency stop.
func (e *Engine) Converse(ctx context.Context, system, userMsg, sessionID string, tools []feature.Tool, gate feature.Gate) (string, error) {
	if !e.Brain.Enabled {
		return "", fmt.Errorf("brain disabled")
	}
	// Inject the SESSION-BOUND on-demand history tool (ADR 0003 D3) so the brain can
	// search THIS conversation's durable past for facts beyond the recent window.
	// Bound to sessionID here → it can never read another session's messages.
	if sessionID != "" && e.Store != nil {
		tools = append(append([]feature.Tool(nil), tools...), features.HistoryTool(e.Store.Reader(), sessionID))
	}
	loop := &toolloop.Loop{
		System:  system, // caller-owned preamble (persona + command hints + tool guidance)
		Tools:   tools,
		Policy:  gate.Mode,    // mutating-tool policy (auto/confirm/off); nil ⇒ off
		Confirm: gate.Confirm, // confirm-mode approval channel; nil ⇒ confirm degrades to off
		Invoke: func(ctx context.Context, prompt string) (string, error) {
			if e.Redact != nil {
				prompt, _ = e.Redact.Scrub(prompt)
			}
			res := brain.Invoke(ctx, brain.Config{Profile: e.Brain.Profile, Model: e.Brain.Model}, prompt, e.Brain.Runner)
			e.meterBrainCall(approxTokens(prompt, res.Raw))
			if res.Billing {
				return "", fmt.Errorf("brain: billing/quota")
			}
			// Malformed is EXPECTED — the loop parses its own protocol, not a
			// StepResult; res.Raw is the text it needs. Only a real transport/exec
			// failure is fatal.
			if res.Err != nil && !res.Malformed {
				return "", res.Err
			}
			return res.Raw, nil
		},
	}
	out, err := loop.Run(ctx, userMsg)
	if err != nil {
		return "", err
	}
	// Re-scrub the ANSWER before it leaves for Telegram: the loop can surface raw
	// tool output (a bestEffort dump, or a model final that echoes a tool result),
	// which was scrubbed on the way INTO the model but not on the way OUT. Same
	// guard as WorkerDiff before it posts.
	if e.Redact != nil {
		out, _ = e.Redact.Scrub(out)
	}
	return out, nil
}
