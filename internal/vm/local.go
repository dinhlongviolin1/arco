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
// docs/herdr-contract.md). Still default=Fake, and two integration items remain
// before this is the daemon default: (1) worker↔agent correlation — herdr
// identifies agents by workspace_id/pane_id, not arco's "arco_<ulid>" workspace,
// so Prompt/Kill's target must be the herdr pane_id captured at launch; (2) the
// launch itself (`herdr agent start <name> --kind <kind> --pane <id> -- <args>`)
// is not yet wired (arco currently only prompts an existing pane).
//
// DO NOT set use_local_vm before (1) is wired: the sweep looks up liveness by
// w.Workspace ("arco_<ulid>") against herdr's workspace_id ("wB"), which NEVER
// matches, so every live worker would miss and be false-finalized Lost/Failed.
// It is not merely inert — enabling it early actively nukes the fleet.
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
	out, err := runOutput(ctx, l.Herdr, "agent", "list")
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
	return newCmd(ctx, l.Herdr, "agent", "prompt", workspace, text).Run()
}

// Kill interrupts an agent (best-effort Ctrl-C).
func (l *LocalVMClient) Kill(ctx context.Context, workspace string) error {
	return newCmd(ctx, l.Herdr, "agent", "send-keys", workspace, "C-c").Run()
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
//
// LIVE-VERIFY CAVEATS (still gated; default stays Fake, use_local_vm guarded):
// (1) that create→list→start works end-to-end on a live server, and (2) whether
// `workspace create --env` REPLACES or merely augments the pane's shell env —
// P1's scrubbed-spawn-env only fully holds if it replaces (else herdr's own env,
// which may carry creds, leaks to the worker). See docs/herdr-contract.md.
func (l *LocalVMClient) Launch(ctx context.Context, spec core.LaunchSpec) (string, error) {
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
	if out, err := runOutput(ctx, l.Herdr, create...); err != nil {
		return "", fmt.Errorf("herdr workspace create: %w: %s", err, out)
	}
	wsID, err := l.workspaceIDByLabel(ctx, spec.Name)
	if err != nil {
		return "", err
	}
	paneID, err := l.paneInWorkspace(ctx, wsID)
	if err != nil {
		return "", err
	}
	start := append([]string{"agent", "start", spec.Name, "--kind", kind, "--pane", paneID, "--"}, spec.Args...)
	if out, err := runOutput(ctx, l.Herdr, start...); err != nil {
		return "", fmt.Errorf("herdr agent start: %w: %s", err, out)
	}
	return paneID, nil
}

// workspaceIDByLabel finds the workspace_id of the (most recent) workspace with
// the given label, from the confirmed `workspace list` envelope.
func (l *LocalVMClient) workspaceIDByLabel(ctx context.Context, label string) (string, error) {
	out, err := runOutput(ctx, l.Herdr, "workspace", "list")
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
	out, err := runOutput(ctx, l.Herdr, "pane", "list")
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

// runOutput runs a command capturing stdout, folding stderr into the error.
func runOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := newCmd(ctx, name, args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), bytes.TrimSpace(ee.Stderr), err)
		}
		return nil, err
	}
	return out, nil
}
