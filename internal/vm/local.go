package vm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/spawnenv"
)

// maxPatchBytes caps how much of a diff patch we buffer/serve, so one fat
// worktree (vendored trees, a committed blob) can't OOM the daemon.
const maxPatchBytes = 1 << 20 // 1 MiB

// terminalHerdrStatus is the set of herdr agent statuses that mean "not alive".
// CONFIRMED against herdr 0.7.5 (Task-S spike): the agent_status enum is exactly
// idle|working|blocked|done|unknown — only `done` means the agent finished;
// idle/working/blocked/unknown are alive (a pane absent from the list = gone).
// NB: a pane that PERSISTS as "unknown" stays alive; its cleanup relies on the
// pane eventually dropping from `agent list` (real process death), not on status.
var terminalHerdrStatus = map[string]bool{"done": true}

// LocalVMClient drives agents on the local machine: git for HEAD/diff (real,
// deterministic) and the herdr CLI over its socket API for liveness + prompting.
// The herdr JSON mapping is CONFIRMED against herdr 0.7.5 (Task-S spike; see
// docs/herdr-contract.md). LIVE-VERIFIED on a real host: the repo-spawn
// create→list→start chain works, and the sweep correlates a Launch-spawned
// worker by its AgentRef (herdr pane_id, captured via BindLaunch). herdrRun
// treats a herdr error envelope (exit 0) as an error, and Launch tears down the
// workspace it created on any post-create failure (no orphan).
//
// KNOWN GAPs (not blockers for the repo-spawn path; use_local_vm is now used):
//   - a launch-ERROR liveness fallback + the legacy Prompt-model path correlate
//     by workspace label ("arco_<ulid>"), which never matches herdr's
//     workspace_id — so a NON-Launch-correlated live worker isn't adopted;
//   - a spawned agent still needs SCOPED credentials (spawnenv strips the
//     inherited key by design) — the provider-pool→clavis-profile wiring is the
//     open worker-auth item. See docs/herdr-contract.md.
type LocalVMClient struct {
	Herdr string
	Git   string
}

var _ core.VMClient = (*LocalVMClient)(nil)

// NewLocal builds a LocalVMClient with default binaries.
func NewLocal(herdr string) *LocalVMClient {
	if herdr == "" {
		herdr = "herdr"
	}
	return &LocalVMClient{Herdr: herdr, Git: "git"}
}

// herdrListResp / herdrAgent mirror the CONFIRMED herdr 0.7.5 `agent list`
// output (Task-S spike): a wrapped envelope, NOT a bare array, and no boot_id/
// pid_start_time (herdr exposes no PID identity). See docs/herdr-contract.md.
//
//	{"result":{"agents":[{"agent":"claude","agent_status":"idle",
//	 "pane_id":"wB:p1","workspace_id":"wB","terminal_id":"term_…", …}],
//	 "type":"agent_list"}}
type herdrListResp struct {
	Result struct {
		Agents []herdrAgent `json:"agents"`
	} `json:"result"`
}

type herdrAgent struct {
	Agent       string `json:"agent"`        // agent kind, e.g. "claude"
	AgentStatus string `json:"agent_status"` // idle|working|blocked|done|unknown
	PaneID      string `json:"pane_id"`      // "wB:p1"
	WorkspaceID string `json:"workspace_id"` // "wB"
	TerminalID  string `json:"terminal_id"`  // stable per-pane identity (PID-reuse guard)
}

// ListAgents returns observed agents. Presence = alive unless the status is a
// terminal one. Empty stdout is an empty list (not an error).
func (l *LocalVMClient) ListAgents(ctx context.Context) ([]core.AgentObs, error) {
	// `agent list` (NOT `--json` — that flag is rejected; list is JSON by default).
	out, err := l.herdrRun(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var resp herdrListResp
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("herdr agent list: bad json: %w", err)
	}
	obs := make([]core.AgentObs, 0, len(resp.Result.Agents))
	for _, a := range resp.Result.Agents {
		// herdr has no boot_id/pid_start_time; terminal_id is the stable per-pane
		// identity, so it carries the PID-reuse guard (AgentObs.BootID slot).
		obs = append(obs, core.AgentObs{
			Ref: a.PaneID, Workspace: a.WorkspaceID, BootID: a.TerminalID,
			Alive: !terminalHerdrStatus[a.AgentStatus],
		})
	}
	return obs, nil
}

// GitHeads returns each worktree's HEAD; bad/empty worktrees are omitted.
func (l *LocalVMClient) GitHeads(ctx context.Context, worktrees []string) (map[string]string, error) {
	heads := make(map[string]string, len(worktrees))
	for _, wt := range worktrees {
		if err := ctx.Err(); err != nil {
			return heads, err // don't report "0 heads" as success on a cancelled sweep
		}
		if wt == "" {
			continue // never `git -C "" ` (would run in the daemon's cwd)
		}
		out, err := newCmd(ctx, l.Git, "-C", wt, "rev-parse", "HEAD").Output()
		if err != nil {
			continue
		}
		heads[wt] = strings.TrimSpace(string(out))
	}
	return heads, nil
}

// Prompt submits a prompt to an agent. NOTE: blocks until herdr returns (bounded
// by ctx). The Task-S spike should add a `--` end-of-options guard so a prompt
// beginning with `-` can't be parsed as a flag.
func (l *LocalVMClient) Prompt(ctx context.Context, workspace, text string) error {
	// Via herdrRun so an exit-0 error envelope (e.g. a bad target) is a real error,
	// not a silent no-op — Prompt is on the live task-delivery + run_again path.
	_, err := l.herdrRun(ctx, "agent", "prompt", workspace, text)
	return err
}

// Kill STOPS an agent and reclaims its pane by closing the herdr workspace it
// runs in — derived from the pane_id target ("wS:pN" → workspace_id "wS"). This
// terminates the process + frees the PTY while leaving arco's SEPARATE worktree
// dir intact (a killed worker's work product survives for inspection). target is
// the worker's AgentRef (pane_id); a non-pane target (Fake / prompt-path worker
// with no captured pane) is a best-effort no-op — there is nothing to close.
func (l *LocalVMClient) Kill(ctx context.Context, target string) error {
	wsID, _, found := strings.Cut(target, ":")
	if !found || wsID == "" {
		return nil
	}
	_, err := l.herdrRun(ctx, "workspace", "close", wsID)
	return err
}

// PromptReady delivers the initial prompt to a JUST-LAUNCHED agent reliably.
// `herdr agent prompt --wait` requires an observed state change (the agent
// reacting) within the timeout, else returns agent_prompt_stalled — so a prompt
// sent while the TUI is still booting is reported as not-landed. A too-early
// prompt is DROPPED by the not-yet-ready TUI (observed: the input stays empty),
// not buffered, so retrying can't double-submit. We retry until the agent starts
// working (confirmed delivery) or the attempts are exhausted; each stalled --wait
// (~promptReadyTimeout) doubles as the settle between tries.
func (l *LocalVMClient) PromptReady(ctx context.Context, workspace, text string) error {
	var last error
	for i := 0; i < promptReadyAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := l.herdrRun(ctx, "agent", "prompt", workspace, text,
			"--wait", "--until", "working", "--timeout", promptReadyTimeout)
		if err == nil {
			return nil // the agent reacted → the prompt landed
		}
		last = err
		if !retryablePromptErr(err) {
			return err // a real error (bad target, etc.) — don't spin
		}
		// Before re-submitting on a stall, check the agent DIDN'T actually react:
		// --wait can report agent_prompt_stalled even when the prompt landed if the
		// working transition wasn't observed within the window (herdr poll lag). Only
		// a still-idle agent is safe to re-prompt — else we'd double-submit the task.
		// (Safety rests on two premises: a too-early prompt is DROPPED by the not-ready
		// TUI, and a landed prompt leaves the agent observably non-idle here.)
		if st := l.agentStatusOf(ctx, workspace); st != "" && st != "idle" && st != "unknown" {
			return nil // reacted (working/blocked/…) — treat as delivered, don't re-type
		}
	}
	return fmt.Errorf("prompt not confirmed after %d attempts: %w", promptReadyAttempts, last)
}

const (
	promptReadyAttempts = 5
	promptReadyTimeout  = "6000" // ms per --wait attempt; a stall (~5s) doubles as the settle
)

// agentStatusOf returns an agent's herdr agent_status ("" if unknown/absent) —
// used to tell a landed-but-unobserved prompt from a genuinely-dropped one.
func (l *LocalVMClient) agentStatusOf(ctx context.Context, target string) string {
	out, err := l.herdrRun(ctx, "agent", "get", target)
	if err != nil {
		return ""
	}
	var resp struct {
		Result struct {
			Agent struct {
				AgentStatus string `json:"agent_status"`
			} `json:"agent"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &resp) != nil {
		return ""
	}
	return resp.Result.Agent.AgentStatus
}

// retryablePromptErr is a herdr "the agent wasn't ready" prompt outcome (still
// booting), as opposed to a hard error (unknown target, etc.).
func retryablePromptErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "stall") || strings.Contains(s, "timeout") || strings.Contains(s, "not ready")
}

// Launch starts a new agent via the herdr contract (all confirmed against herdr
// 0.7.5, Task-S spike):
//
//	herdr workspace create --no-focus --label <name> --cwd <workdir> [--env …]
//	→ resolve the new workspace_id by label (confirmed `workspace list` shape)
//	→ resolve its pane_id (confirmed `pane list` shape)
//	→ herdr agent start <name> --kind <kind> --pane <pane_id> -- <args…>
//
// The returned ref is the pane_id (matches ListAgents' Ref, so the sweep
// correlates). --no-focus avoids stealing the operator's view. IDs are parsed
// only from the read-only list envelopes (confirmed shapes — NOT the create
// responses), so this doesn't repeat the assumed-schema bug the spike fixed.
// LIVE-VERIFIED end-to-end on herdr 0.7.5 (create→list→start; agent correlated by
// pane_id). herdrRun catches exit-0 error envelopes; a post-create failure closes
// the workspace so a failed launch never orphans one.

// herdrAgentName maps arco's workspace label ("arco_<ULID>", where the Crockford
// ULID is UPPERCASE) to a name herdr's `agent start` accepts: 1–32 chars,
// starting with a lowercase letter, only [a-z0-9_-]. Lowercasing satisfies the
// rule while keeping the ULID unique. (`workspace create --label` is laxer and
// takes the original; correlation is by pane_id, not this name.)
func herdrAgentName(label string) string { return strings.ToLower(label) }

func (l *LocalVMClient) Launch(ctx context.Context, spec core.LaunchSpec) (string, string, error) {
	kind := spec.Kind
	if kind == "" {
		kind = "claude"
	}
	create := []string{"workspace", "create", "--no-focus", "--label", spec.Name}
	if spec.Workdir != "" {
		create = append(create, "--cwd", spec.Workdir)
	}
	for _, kv := range spec.Env { // scrubbed env for the launched process (P1)
		create = append(create, "--env", kv)
	}
	// `workspace create` is the ONLY herdr call carrying secret `--env` values on
	// its argv, so its error is the only one that can echo a token (via argv,
	// herdr stderr, or an error envelope). Scrub the actual secret VALUES from the
	// whole error text — a herdr-echo-independent close of the HIGH-1 residual.
	secrets := secretEnvValues(spec.Env)
	if _, err := l.herdrRun(ctx, create...); err != nil {
		return "", "", redactSecrets(fmt.Errorf("herdr workspace create: %w", err), secrets)
	}
	// Every post-create error return is wrapped with redactSecrets(…, secrets): the
	// workspace now holds the injected env, so if herdr ever echoed a pane's stored
	// env value on a later failure the value-scrubber catches it — the guarantee
	// stays echo-independent for the whole launch, not just the create call (review).
	wsID, err := l.workspaceIDByLabel(ctx, spec.Name)
	if err != nil {
		// The workspace was created but we can't resolve its id to clean it up
		// (rare). Return the error; a sweep-GC follow-up reaps the orphan.
		return "", "", redactSecrets(err, secrets)
	}
	// From here the workspace exists → tear it down on ANY failure so a failed
	// launch never orphans a live herdr workspace (capstone audit; no GC today).
	paneID, err := l.paneInWorkspace(ctx, wsID)
	if err != nil {
		l.closeWorkspace(ctx, wsID)
		return "", "", redactSecrets(err, secrets)
	}
	// herdr `agent start <name>` requires 1–32 chars, starting with a lowercase
	// letter and containing only [a-z0-9_-]. arco's workspace label embeds an
	// UPPERCASE Crockford ULID ("arco_01KZ…"), which `workspace create --label`
	// accepts but `agent start` rejects (invalid_agent_name — live-verified on
	// herdr 0.7.5). Lowercase it: the ULID stays unique, and arco correlates the
	// worker by the returned pane_id (AgentRef), never by this agent name.
	start := []string{"agent", "start", herdrAgentName(spec.Name), "--kind", kind, "--pane", paneID}
	if len(spec.Args) > 0 { // only add the `--` marker when args follow (off-contract otherwise)
		start = append(append(start, "--"), spec.Args...)
	}
	if _, err := l.herdrRun(ctx, start...); err != nil {
		l.closeWorkspace(ctx, wsID) // don't orphan the workspace on a failed agent start
		return "", "", redactSecrets(fmt.Errorf("herdr agent start: %w", err), secrets)
	}
	// Resolve the just-started agent's stable identity (terminal_id) so the worker
	// row carries it from birth — arming the sweep's identity guard before the
	// first liveness observation (a recycled pane_id then can't be adopted as this
	// worker's). Best-effort: "" if the agent isn't listed yet (rare startup race);
	// the guard just falls back to identity-on-first-observe, and the reaper safely
	// declines an unidentifiable agent.
	return paneID, l.terminalIDForPane(ctx, paneID), nil
}

// terminalIDForPane returns the herdr terminal_id of the agent on paneID ("" if
// none / unresolvable). terminal_id is herdr's stable per-pane identity (the
// PID-reuse guard); see ListAgents.
func (l *LocalVMClient) terminalIDForPane(ctx context.Context, paneID string) string {
	obs, err := l.ListAgents(ctx)
	if err != nil {
		return ""
	}
	for _, o := range obs {
		if o.Ref == paneID {
			return o.BootID
		}
	}
	return ""
}

// closeWorkspace best-effort tears down a workspace (used to avoid orphaning one
// after a partial launch). A close failure is swallowed — the caller is already
// returning a launch error, and the orphan (if any) awaits the sweep-GC follow-up.
func (l *LocalVMClient) closeWorkspace(ctx context.Context, wsID string) {
	_, _ = l.herdrRun(ctx, "workspace", "close", wsID)
}

// herdrRun runs a herdr command and fails on BOTH a non-zero exit AND a herdr
// error envelope returned WITH exit 0 (`{"error":{...}}`) — herdr does the latter
// for some errors (e.g. invalid_agent_name), so a clean exit code alone is not
// success. Centralizes the check for every herdr call (git calls stay on runOutput).
func (l *LocalVMClient) herdrRun(ctx context.Context, args ...string) ([]byte, error) {
	out, err := runOutput(ctx, l.Herdr, args...)
	if err != nil {
		return nil, err
	}
	if e := herdrEnvelopeError(out); e != nil {
		return nil, fmt.Errorf("%s: %w", redactCmdArgs(args), e)
	}
	return out, nil
}

// herdrEnvelopeError returns a non-nil error if out is a herdr error envelope
// ({"error":{"code","message"}}). Non-envelope / non-JSON output yields nil (the
// caller's own parser handles a malformed body).
func herdrEnvelopeError(out []byte) error {
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(out, &env) == nil && env.Error != nil {
		return fmt.Errorf("herdr error %s: %s", env.Error.Code, env.Error.Message)
	}
	return nil
}

// workspaceIDByLabel finds the workspace_id of the (most recent) workspace with
// the given label, from the confirmed `workspace list` envelope.
func (l *LocalVMClient) workspaceIDByLabel(ctx context.Context, label string) (string, error) {
	out, err := l.herdrRun(ctx, "workspace", "list")
	if err != nil {
		return "", fmt.Errorf("herdr workspace list: %w", err)
	}
	var resp struct {
		Result struct {
			Workspaces []struct {
				WorkspaceID string `json:"workspace_id"`
				Label       string `json:"label"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("herdr workspace list: bad json: %w", err)
	}
	id := ""
	for _, w := range resp.Result.Workspaces {
		if w.Label == label {
			id = w.WorkspaceID // last match wins (most recently created)
		}
	}
	if id == "" {
		return "", fmt.Errorf("herdr: no workspace with label %q after create", label)
	}
	return id, nil
}

// paneInWorkspace returns a pane_id belonging to workspace wsID, from the
// confirmed `pane list` envelope.
func (l *LocalVMClient) paneInWorkspace(ctx context.Context, wsID string) (string, error) {
	out, err := l.herdrRun(ctx, "pane", "list")
	if err != nil {
		return "", fmt.Errorf("herdr pane list: %w", err)
	}
	var resp struct {
		Result struct {
			Panes []struct {
				PaneID      string `json:"pane_id"`
				WorkspaceID string `json:"workspace_id"`
			} `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("herdr pane list: bad json: %w", err)
	}
	for _, p := range resp.Result.Panes {
		if p.WorkspaceID == wsID {
			return p.PaneID, nil
		}
	}
	return "", fmt.Errorf("herdr: no pane in workspace %q", wsID)
}

// Diff returns the base→head numstat summary + a size-capped patch.
func (l *LocalVMClient) Diff(ctx context.Context, worktree, base, head string) (core.Diff, error) {
	d := core.Diff{Base: base, Head: head}
	if worktree == "" || base == "" || head == "" {
		return d, nil
	}
	// Never let a DB rev that starts with '-' (or is otherwise not commit-shaped)
	// be parsed by git as an option (e.g. --output=/path zeroes the diff).
	if !looksLikeRev(base) || !looksLikeRev(head) {
		return d, fmt.Errorf("vm: refusing non-commit-shaped rev (base=%q head=%q)", base, head)
	}
	rng := base + ".." + head
	num, err := newCmd(ctx, l.Git, "-C", worktree, "diff", "--numstat", rng).Output()
	if err != nil {
		return d, err
	}
	sc := bufio.NewScanner(bytes.NewReader(num))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		d.Files++
		if n, err := strconv.Atoi(fields[0]); err == nil {
			d.Insertions += n
		}
		if n, err := strconv.Atoi(fields[1]); err == nil {
			d.Deletions += n
		}
	}
	if err := sc.Err(); err != nil {
		return d, fmt.Errorf("herdr diff numstat scan: %w", err)
	}
	d.Patch, d.Truncated = l.cappedPatch(ctx, worktree, rng)
	return d, nil
}

// cappedPatch streams `git diff` and buffers at most maxPatchBytes, draining the
// rest so git never blocks on a full pipe.
func (l *LocalVMClient) cappedPatch(ctx context.Context, worktree, rng string) (string, bool) {
	cmd := newCmd(ctx, l.Git, "-C", worktree, "diff", rng)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false
	}
	if err := cmd.Start(); err != nil {
		return "", false
	}
	b, _ := io.ReadAll(io.LimitReader(stdout, maxPatchBytes+1))
	_, _ = io.Copy(io.Discard, stdout) // drain remainder
	_ = cmd.Wait()
	if len(b) > maxPatchBytes {
		return string(b[:maxPatchBytes]) + "\n...[patch truncated]", true
	}
	return string(b), false
}

// looksLikeRev reports whether s is a plausible git commit (hex, 7..64 chars) —
// enough to reject option-injection without a full git check-ref-format.
func looksLikeRev(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// newCmd builds an exec.Cmd whose environment has arco's high-blast credentials
// stripped (security precondition P1) — no subprocess arco launches (git, herdr,
// and the worker agent when launched here) inherits arco's provider keys/tokens/
// own config. NB: the worker AGENT is currently spawned by herdr/clavis, not
// arco; that launch path MUST apply the same scrub — this covers every process
// arco spawns directly (defense-in-depth).
func newCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, args...)
	c.Env = spawnenv.Scrub(os.Environ())
	return c
}

// redactCmdArgs renders argv for an ERROR MESSAGE with secret env values masked.
// The launch path passes a worker's SCOPED provider creds to herdr via `--env
// KEY=VALUE` (herdr's only pane-env mechanism), so an unmasked argv in an error
// string would carry a LIVE token into the immutable event log (Spawn writes the
// launch error into dispatch_done) and thence into the brain's LLM context — a
// P1/B4 bypass the shape-based write-time redactor can't catch for an arbitrary
// gateway token (whole-system audit). We mask the value of any `--env` operand
// whose key is secret-bearing (spawnenv's denylist), keeping benign env visible.
func redactCmdArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if i > 0 && args[i-1] == "--env" {
			if k, _, ok := strings.Cut(a, "="); ok && spawnenv.IsSecretVar(k) {
				out[i] = k + "=<redacted>"
			}
		}
	}
	return strings.Join(out, " ")
}

// secretEnvValues extracts the VALUES of secret-bearing `KEY=VALUE` env entries
// (spawnenv's denylist) — the exact strings that must never surface in an error.
func secretEnvValues(env []string) []string {
	var vals []string
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" && spawnenv.IsSecretVar(k) {
			vals = append(vals, v)
		}
	}
	return vals
}

// redactedError wraps an error, masking known secret substrings in its rendered
// message while preserving the wrap chain. This closes the error-text leak paths
// argv-masking can't reach: a herdr STDERR line (runOutput folds `ee.Stderr`) or
// an error-envelope MESSAGE that echoes a `--env` VALUE (audit HIGH-1 residual —
// the guarantee must not depend on whether herdr echoes the value).
type redactedError struct {
	err     error
	secrets []string
}

func (r *redactedError) Error() string {
	s := r.err.Error()
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, "<redacted>")
	}
	return s
}
func (r *redactedError) Unwrap() error { return r.err }

// redactSecrets wraps err so any of the given secret values are masked wherever
// they appear in its message. No-op when there is nothing to redact.
func redactSecrets(err error, secrets []string) error {
	if err == nil || len(secrets) == 0 {
		return err
	}
	return &redactedError{err: err, secrets: secrets}
}

// runOutput runs a command capturing stdout, folding stderr into the error.
func runOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := newCmd(ctx, name, args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s %s: %s: %w", name, redactCmdArgs(args), bytes.TrimSpace(ee.Stderr), err)
		}
		return nil, err
	}
	return out, nil
}
