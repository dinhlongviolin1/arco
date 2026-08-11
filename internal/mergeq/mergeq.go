// Package mergeq is verification leg 2 (rev7/T3.2): an arco-owned, in-daemon
// merge queue. Candidate work is integrated into the target repo's main
// SERIALLY — clone the target, merge the worker's head, optionally run a test
// gate, push — and a merge that lands is recorded as verification EVIDENCE
// (one deduped verification_artifact). It never auto-verifies: the human
// diff-gate in verify.go stays the only path to completed_verified.
//
// The queue is EVENT-SOURCED on the ledger (mergeq_enqueued / mergeq_merged /
// mergeq_kicked events): a fresh Queue over the same store sees the same items
// and resumes, so a daemon restart can never lose or duplicate queue work.
package mergeq

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/spawnenv"
)

// Item statuses. An item is written once (pending) and finalized once
// (merged|kicked); a kicked worker may be re-enqueued as a NEW item.
const (
	StatusPending = "pending"
	StatusMerged  = "merged"
	StatusKicked  = "kicked"
)

// Item is one queued integration: worker head → repo main.
type Item struct {
	ID       string
	WorkerID string
	Repo     string
	Head     string
	Status   string
}

// Config tunes the queue. GitBin "" means "git". An empty TestCmd skips the
// test gate; otherwise it runs in the integration workspace and a non-zero
// exit kicks the item back.
type Config struct {
	GitBin  string
	TestCmd []string
}

// Queue is the in-daemon merge queue. It holds no state of its own — every
// read reconstructs from the ledger, every outcome is a ledger event.
type Queue struct {
	s   core.Store
	cfg Config
}

// New builds a Queue over the store.
func New(s core.Store, cfg Config) *Queue { return &Queue{s: s, cfg: cfg} }

// maxDetail caps git/test output persisted into an escalation detail.
const maxDetail = 2000

// itemPayload is the JSON body of every mergeq_* event. The item id is the
// join key; repo/head are carried on the enqueue event only.
type itemPayload struct {
	Item   string `json:"item"`
	Repo   string `json:"repo,omitempty"`
	Head   string `json:"head,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Enqueue queues a worker's head for integration into its target repo's main.
// The target is the worktree clone's `origin` remote (the worker row has no
// repo field). One PENDING item per worker: re-enqueueing returns the
// existing item id. Unknown worker → error.
func (q *Queue) Enqueue(ctx context.Context, workerID string) (string, error) {
	w, err := q.s.Reader().GetWorker(workerID)
	if err != nil {
		return "", fmt.Errorf("mergeq: enqueue %s: %w", workerID, err)
	}
	if w.Worktree == "" || w.HeadCommit == "" {
		return "", fmt.Errorf("mergeq: worker %s has no worktree/head to integrate", workerID)
	}
	repo, out, err := q.git(ctx, w.Worktree, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("mergeq: resolve origin of %s: %v: %s", w.Worktree, err, out)
	}
	var id string
	err = q.s.WithTx(ctx, func(tx core.Tx) error {
		// Pending-dedup inside the single-writer tx, so two racing enqueues can
		// never mint two pending items for one worker.
		items, err := loadItems(tx)
		if err != nil {
			return err
		}
		for _, it := range items {
			if it.WorkerID == workerID && it.Status == StatusPending {
				id = it.ID
				return nil
			}
		}
		id = ulid.Make().String()
		payload, _ := json.Marshal(itemPayload{Item: id, Repo: repo, Head: w.HeadCommit})
		_, _, _, err = tx.AppendEvent(core.Event{
			Kind: "mergeq_enqueued", WorkerID: workerID, SessionID: w.OwnerSession,
			Actor: "mergeq", Source: "internal", SourceEventID: "mergeq:enqueued:" + id,
			Payload: string(payload),
		})
		return err
	})
	return id, err
}

// Items returns the queue in enqueue (FIFO) order, reconstructed from the
// ledger — the same view a freshly-restarted daemon sees.
func (q *Queue) Items(ctx context.Context) ([]Item, error) {
	return loadItems(q.s.Reader())
}

// ProcessNext integrates the oldest pending item; (false, nil) when the queue
// is empty. Strictly one item per call — serialized processing is the point.
// A kickback (conflict, red tests, denied push) is an OUTCOME recorded on the
// ledger, not an error.
func (q *Queue) ProcessNext(ctx context.Context) (bool, error) {
	items, err := loadItems(q.s.Reader())
	if err != nil {
		return false, err
	}
	var it Item
	found := false
	for _, x := range items {
		if x.Status == StatusPending {
			it, found = x, true
			break
		}
	}
	if !found {
		return false, nil
	}
	w, err := q.s.Reader().GetWorker(it.WorkerID)
	if err != nil {
		return false, fmt.Errorf("mergeq: item %s worker: %w", it.ID, err)
	}
	reason, detail := q.integrate(ctx, it, w.Worktree)
	if reason == "" {
		return true, q.recordMerged(ctx, it, w)
	}
	return true, q.recordKicked(ctx, it, w, reason, detail)
}

// integrate does the actual git work in a SCRATCH clone — the worker's own
// worktree is never mutated. Returns ("", "") when the head landed on origin
// main, else a kick reason + the failing command's output. Any failure leaves
// origin main unmoved (nothing is pushed until every gate passed).
func (q *Queue) integrate(ctx context.Context, it Item, worktree string) (reason, detail string) {
	tmp, err := os.MkdirTemp("", "arco-mergeq-")
	if err != nil {
		return "workspace setup failed", err.Error()
	}
	defer os.RemoveAll(tmp)
	// it.Head is worker-influenced (reconstructed from an intake observed_head).
	// It is now gated at the intake boundary, but re-validate at the git-exec
	// boundary too (defense in depth): a non-commit-shaped head must never reach
	// `fetch`/`merge`, where `--upload-pack=<cmd>` would be code execution.
	if !core.LooksLikeRev(it.Head) {
		return "worker head is not a valid commit id", it.Head
	}
	ws := filepath.Join(tmp, "repo")
	// `-c protocol.allow=user` + `protocol.ext.allow=never` block the `ext::`/
	// remote-helper class of origin-URL command execution; the target repo is an
	// operator-configured origin, but it is read from the worker-writable clone's
	// .git/config, so it is not fully trusted.
	proto := []string{"-c", "protocol.allow=user", "-c", "protocol.ext.allow=never"}
	if _, out, err := q.git(ctx, tmp, append(append([]string{}, proto...), "clone", "--quiet", "--", it.Repo, ws)...); err != nil {
		return "clone of the target repo failed", out
	}
	if _, out, err := q.git(ctx, ws, "checkout", "--quiet", "main"); err != nil {
		return "target repo has no main branch", out
	}
	// Crash-window idempotence: if a previous ProcessNext pushed this head but
	// crashed before recording `merged`, the item is still pending and gets
	// reprocessed. The head is already reachable from origin main, so record it
	// as landed (merged) rather than re-running the gate — which, if it now
	// flaked or main moved, would wrongly kick work that actually succeeded.
	if _, _, err := q.git(ctx, ws, "merge-base", "--is-ancestor", it.Head, "HEAD"); err == nil {
		return "", "" // already on origin main
	}
	// The head is fetched from the worker's worktree (plain path-to-path git),
	// then merged — a clone taken before main moved integrates cleanly instead
	// of failing a non-ff push. `--` ends options so a head can never be read as
	// a flag; the head shape is already validated above.
	if _, out, err := q.git(ctx, ws, append(append([]string{}, proto...), "fetch", "--quiet", "--", worktree, it.Head)...); err != nil {
		return "fetch of the worker head failed", out
	}
	if _, out, err := q.git(ctx, ws, "merge", "--no-edit", "--quiet", it.Head); err != nil {
		return "merge conflict", out
	}
	if len(q.cfg.TestCmd) > 0 {
		cmd := exec.CommandContext(ctx, q.cfg.TestCmd[0], q.cfg.TestCmd[1:]...)
		cmd.Dir = ws
		// The gate runs the WORKER-MERGED tree's build/test scripts — worker-
		// controlled code. Scrub the daemon's creds from its env (P1), exactly
		// like git() above; otherwise a hostile Makefile/test target reads
		// ANTHROPIC_API_KEY/GITHUB_TOKEN straight out of the environment.
		cmd.Env = append(spawnenv.Scrub(os.Environ()), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "test gate failed", string(out)
		}
	}
	// A denied push (e.g. a NON-BARE target refusing its checked-out branch)
	// must surface as a kickback with the git error, never a crash.
	if _, out, err := q.git(ctx, ws, "push", "--quiet", "origin", "HEAD:main"); err != nil {
		return "push to origin main rejected", out
	}
	return "", ""
}

// recordMerged finalizes an item as merged and appends the ONE
// verification_artifact. Both events dedup on (source, source_event_id), so a
// crash-and-reprocess or restart can never duplicate the evidence.
func (q *Queue) recordMerged(ctx context.Context, it Item, w core.Worker) error {
	return q.s.WithTx(ctx, func(tx core.Tx) error {
		payload, _ := json.Marshal(itemPayload{Item: it.ID})
		if _, _, _, err := tx.AppendEvent(core.Event{
			Kind: "mergeq_merged", WorkerID: it.WorkerID, SessionID: w.OwnerSession,
			Actor: "mergeq", Source: "internal", SourceEventID: "mergeq:merged:" + it.ID,
			Payload: string(payload),
		}); err != nil {
			return err
		}
		artifact, _ := json.Marshal(map[string]any{
			"result": "merged", "head": it.Head, "repo": it.Repo, "via": "merge_queue",
		})
		_, _, _, err := tx.AppendEvent(core.Event{
			Kind: "verification_artifact", WorkerID: it.WorkerID, SessionID: w.OwnerSession,
			Actor: "mergeq", Source: "internal",
			SourceEventID: "mergeq:" + it.WorkerID + ":" + it.Head,
			Payload:       string(artifact),
		})
		return err
	})
}

// recordKicked finalizes an item as kicked and opens the confirm escalation
// for the worker. OpenEscalation's one-pending-per-worker dedup keeps repeat
// kicks from piling escalations up.
func (q *Queue) recordKicked(ctx context.Context, it Item, w core.Worker, reason, detail string) error {
	if len(detail) > maxDetail {
		detail = detail[:maxDetail] + "…"
	}
	return q.s.WithTx(ctx, func(tx core.Tx) error {
		payload, _ := json.Marshal(itemPayload{Item: it.ID, Reason: reason})
		if _, _, _, err := tx.AppendEvent(core.Event{
			Kind: "mergeq_kicked", WorkerID: it.WorkerID, SessionID: w.OwnerSession,
			Actor: "mergeq", Source: "internal", SourceEventID: "mergeq:kicked:" + it.ID,
			Payload: string(payload),
		}); err != nil {
			return err
		}
		_, err := tx.OpenEscalation(core.Escalation{
			WorkerID: it.WorkerID, SessionID: w.OwnerSession, Kind: "confirm",
			QuestionClass: "proceed-confirmation", ActionClass: core.ClassAmbiguous, Tier: core.TierMedium,
			ActionFingerprint: "mergeq:" + it.WorkerID + ":" + it.Head,
			Action:            "merge queue: integrating the worker's head into " + it.Repo + " main failed",
			Detail:            reason + ": " + strings.TrimSpace(detail),
		})
		return err
	})
}

// loadItems reconstructs the queue by scanning the event log in id order:
// enqueued events create items (FIFO), merged/kicked events finalize them.
func loadItems(r core.Reader) ([]Item, error) {
	var items []Item
	idx := map[string]int{}
	cursor := int64(0)
	for {
		const batch = 500
		evs, err := r.EventsSince(cursor, batch)
		if err != nil {
			return nil, err
		}
		for _, ev := range evs {
			cursor = ev.ID
			if !strings.HasPrefix(ev.Kind, "mergeq_") {
				continue
			}
			var p itemPayload
			if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil || p.Item == "" {
				continue // a foreign/malformed mergeq event must not poison the queue
			}
			switch ev.Kind {
			case "mergeq_enqueued":
				items = append(items, Item{
					ID: p.Item, WorkerID: ev.WorkerID, Repo: p.Repo, Head: p.Head, Status: StatusPending,
				})
				idx[p.Item] = len(items) - 1
			case "mergeq_merged":
				if i, ok := idx[p.Item]; ok {
					items[i].Status = StatusMerged
				}
			case "mergeq_kicked":
				if i, ok := idx[p.Item]; ok {
					items[i].Status = StatusKicked
				}
			}
		}
		if len(evs) < batch {
			return items, nil
		}
	}
}

// git runs one git command with a pinned committer identity (merge commits
// need one even when the daemon's user has no global git config) and prompts
// disabled (a hung credential prompt must never stall the queue).
func (q *Queue) git(ctx context.Context, dir string, args ...string) (stdout, combined string, err error) {
	bin := q.cfg.GitBin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	// Hardened env, matching worktree.Provision: the daemon's own environment is
	// SCRUBBED (P1 — no arco/provider creds leak into a git subprocess that
	// operates on attacker-influenced repos), and global/system gitconfig is
	// neutered so a machine-level alias/pager/filter/url-insteadOf can't turn a
	// merge into code execution. A pinned committer identity + no prompts round
	// it out. (Per-command protocol/option guards live at the call sites.)
	cmd.Env = append(spawnenv.Scrub(os.Environ()),
		"GIT_AUTHOR_NAME=arco-mergeq", "GIT_AUTHOR_EMAIL=mergeq@arco.local",
		"GIT_COMMITTER_NAME=arco-mergeq", "GIT_COMMITTER_EMAIL=mergeq@arco.local",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	var so, all strings.Builder
	cmd.Stdout = io.MultiWriter(&so, &all)
	cmd.Stderr = &all
	err = cmd.Run()
	return strings.TrimSpace(so.String()), all.String(), err
}
