package reconcile

import (
	"context"
	"fmt"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/brain"
)

// BrainReply runs a one-shot CONVERSATIONAL brain call — the operator chatting
// with arco over Telegram — and returns the model's raw text reply (NOT a
// StepResult; this path does not act on the fleet). The prompt is scrubbed
// before it leaves for the third-party LLM (same exfil guard as classify), and
// the call is metered like any other brain call. A disabled brain is an error
// the caller can surface as "chat unavailable".
func (e *Engine) BrainReply(ctx context.Context, prompt string) (string, error) {
	if !e.Brain.Enabled {
		return "", fmt.Errorf("brain disabled")
	}
	if e.Redact != nil {
		prompt, _ = e.Redact.Scrub(prompt)
	}
	res := brain.Invoke(ctx, brain.Config{Profile: e.Brain.Profile, Model: e.Brain.Model}, prompt, e.Brain.Runner)
	e.meterBrainCall(approxTokens(prompt, res.Raw))
	if res.Billing {
		return "", fmt.Errorf("brain: billing/quota")
	}
	// A chat reply is plain prose, so brain.Invoke reports Malformed (it couldn't
	// parse a StepResult) — that is EXPECTED here: the raw model text IS the
	// answer. Only a real exec/transport failure (Err without Malformed) is a
	// failure for chat.
	if res.Err != nil && !res.Malformed {
		return "", res.Err
	}
	reply := strings.TrimSpace(res.Raw)
	if reply == "" {
		return "", fmt.Errorf("empty reply")
	}
	return reply, nil
}
