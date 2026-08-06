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
// Confirm the real values in the Task-S spike; anything NOT in this set (incl.
// empty/unknown) is treated as alive (presence in the list = alive).
var terminalHerdrStatus = map[string]bool{"done": true, "gone": true, "exited": true, "dead": true}

// LocalVMClient drives agents on the local machine: git for HEAD/diff (real,
// deterministic) and the herdr CLI over its socket API for liveness + prompting.
// The herdr JSON field mapping is against herdr's documented `agent list --json`
// / `agent prompt` contract; exact fields/types must be confirmed against a live
// herdr in the Task-S spike before this is the daemon default (default = Fake).
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

// herdrAgent mirrors the fields we consume. Identity fields are RawMessage so a
// numeric OR string encoding both decode (herdr's exact types are spike-TBD).
type herdrAgent struct {
	Workspace string          `json:"workspace"`
	Label     string          `json:"label"`
	Status    string          `json:"status"`
	BootID    json.RawMessage `json:"boot_id"`
	PIDStart  json.RawMessage `json:"pid_start_time"`
}

func rawStr(r json.RawMessage) string {
	s := strings.TrimSpace(string(r))
	if s == "" || s == "null" {
		return ""
	}
	if len(s) >= 2 && s[0] == '"' {
		var out string
		if json.Unmarshal(r, &out) == nil {
			return out
		}
	}
	return s
}

// ListAgents returns observed agents. Presence = alive unless the status is a
// terminal one. Empty stdout is an empty list (not an error).
func (l *LocalVMClient) ListAgents(ctx context.Context) ([]core.AgentObs, error) {
	out, err := runOutput(ctx, l.Herdr, "agent", "list", "--json")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var raw []herdrAgent
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("herdr agent list: bad json: %w", err)
	}
	obs := make([]core.AgentObs, 0, len(raw))
	for _, a := range raw {
		ws := a.Workspace
		if ws == "" {
			ws = a.Label
		}
		obs = append(obs, core.AgentObs{
			Workspace: ws, BootID: rawStr(a.BootID), PIDStartTime: rawStr(a.PIDStart),
			Alive: !terminalHerdrStatus[a.Status],
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
