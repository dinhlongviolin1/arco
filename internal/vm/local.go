package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// LocalVMClient drives agents on the local machine: git for HEAD/diff (real,
// deterministic) and the herdr CLI over its socket API for agent liveness +
// prompting. The herdr JSON field mapping below is against herdr's documented
// `agent list --json` / `agent prompt` contract; the exact fields must be
// confirmed against a running herdr in the Task-S spike before this is the
// daemon default (the daemon still uses Fake until then).
type LocalVMClient struct {
	Herdr string // herdr binary (default "herdr")
	Git   string // git binary (default "git")
}

var _ core.VMClient = (*LocalVMClient)(nil)

// NewLocal builds a LocalVMClient with default binaries.
func NewLocal(herdr string) *LocalVMClient {
	if herdr == "" {
		herdr = "herdr"
	}
	return &LocalVMClient{Herdr: herdr, Git: "git"}
}

// herdrAgent mirrors the fields we consume from `herdr agent list --json`.
type herdrAgent struct {
	Workspace string `json:"workspace"`
	Label     string `json:"label"`
	Status    string `json:"status"` // working|idle|blocked|done|unknown
	BootID    string `json:"boot_id"`
	PIDStart  string `json:"pid_start_time"`
}

// ListAgents returns observed agents. Presence in the list = alive; identity
// fields (boot_id, pid_start_time) are passed through when herdr provides them.
func (l *LocalVMClient) ListAgents(ctx context.Context) ([]core.AgentObs, error) {
	out, err := exec.CommandContext(ctx, l.Herdr, "agent", "list", "--json").Output()
	if err != nil {
		return nil, err
	}
	var raw []herdrAgent
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	obs := make([]core.AgentObs, 0, len(raw))
	for _, a := range raw {
		ws := a.Workspace
		if ws == "" {
			ws = a.Label
		}
		obs = append(obs, core.AgentObs{
			Workspace: ws, BootID: a.BootID, PIDStartTime: a.PIDStart,
			Alive: a.Status != "" && a.Status != "gone",
		})
	}
	return obs, nil
}

// GitHeads returns each worktree's current HEAD (git rev-parse). Worktrees that
// error (missing/not a repo) are omitted rather than failing the whole sweep.
func (l *LocalVMClient) GitHeads(ctx context.Context, worktrees []string) (map[string]string, error) {
	heads := make(map[string]string, len(worktrees))
	for _, wt := range worktrees {
		out, err := exec.CommandContext(ctx, l.Git, "-C", wt, "rev-parse", "HEAD").Output()
		if err != nil {
			continue
		}
		heads[wt] = strings.TrimSpace(string(out))
	}
	return heads, nil
}

// Prompt submits a prompt to an agent (fire-and-return; delivery is later proven
// by the normalizer observing the embedded intent ULID).
func (l *LocalVMClient) Prompt(ctx context.Context, workspace, text string) error {
	return exec.CommandContext(ctx, l.Herdr, "agent", "prompt", workspace, text).Run()
}

// Kill interrupts an agent (best-effort Ctrl-C via send-keys).
func (l *LocalVMClient) Kill(ctx context.Context, workspace string) error {
	return exec.CommandContext(ctx, l.Herdr, "agent", "send-keys", workspace, "C-c").Run()
}

// Diff returns the base→head numstat summary + patch for a worktree.
func (l *LocalVMClient) Diff(ctx context.Context, worktree, base, head string) (core.Diff, error) {
	d := core.Diff{Base: base, Head: head}
	if base == "" || head == "" {
		return d, nil
	}
	rng := base + ".." + head
	num, err := exec.CommandContext(ctx, l.Git, "-C", worktree, "diff", "--numstat", rng).Output()
	if err != nil {
		return d, err
	}
	sc := bufio.NewScanner(strings.NewReader(string(num)))
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
	// Full patch (best-effort; empty on error is fine for the summary).
	if patch, err := exec.CommandContext(ctx, l.Git, "-C", worktree, "diff", rng).Output(); err == nil {
		d.Patch = string(patch)
	}
	return d, nil
}
