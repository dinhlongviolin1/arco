package brain

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// Runner executes an external command and returns its combined stdout. Injected
// so tests never shell out. DefaultRunner shells out to clavis.
type Runner func(ctx context.Context, name string, args ...string) (stdout []byte, err error)

// DefaultRunner runs the command for real (used by the daemon).
func DefaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Config selects the clavis profile + model for a brain call.
type Config struct {
	Profile string
	Model   string
	Timeout time.Duration
}

// InvokeResult is the outcome of one brain call. Exactly one of {Step valid,
// Malformed, Billing, Err} characterizes it.
type InvokeResult struct {
	Step      core.StepResult
	Raw       string
	Malformed bool
	Billing   bool // provider balance/quota wall — caller must NOT retry
	Err       error
}

// Invoke runs one short-lived clavis call and parses a StepResult. It never
// passes --max-tokens (a reasoning model could burn the cap and truncate the
// JSON) and classifies a billing wall so the caller parks instead of retrying.
func Invoke(ctx context.Context, cfg Config, prompt string, run Runner) InvokeResult {
	if run == nil {
		run = DefaultRunner
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// clavis <profile> -- --model <model> -p <prompt>   (no --max-tokens)
	args := []string{cfg.Profile, "--", "--model", cfg.Model, "-p", prompt}
	out, err := run(cctx, "clavis", args...)
	raw := string(out)
	if err != nil {
		if isBilling(raw) || isBilling(err.Error()) {
			return InvokeResult{Raw: raw, Billing: true, Err: err}
		}
		return InvokeResult{Raw: raw, Err: err}
	}
	if isBilling(raw) {
		return InvokeResult{Raw: raw, Billing: true}
	}
	step, perr := ParseStep(raw)
	if perr != nil {
		return InvokeResult{Raw: raw, Malformed: true, Err: perr}
	}
	return InvokeResult{Step: step, Raw: raw}
}

func isBilling(s string) bool {
	s = strings.ToLower(s)
	for _, needle := range []string{"insufficient balance", "insufficient_quota", "billing",
		"payment required", "402", "quota exceeded", "credit balance"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
