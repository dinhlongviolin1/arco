package reconcile

import (
	"context"
	"encoding/json"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// CIRunner executes the external check-runs query (gh) rooted at dir and
// returns its stdout. Injected so tests never shell out; NewGHRunner is the
// real one.
type CIRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// CICfg gates verification leg 1 (rev7/T3.1): CI check-runs polling for
// completed_candidate workers. The zero value (disabled) is the default; the
// daemon enables it from config with the real gh runner.
type CICfg struct {
	Enabled bool
	Runner  CIRunner
}

// ghCITimeout bounds one gh api call so a hung network can never stall the
// sweep (the poll runs inline in Sweep).
const ghCITimeout = 30 * time.Second

// NewGHRunner returns the real CIRunner: it shells out to gh with the worker's
// worktree as cwd, so gh resolves {owner}/{repo} from that repo's git remote.
func NewGHRunner(bin string) CIRunner {
	if bin == "" {
		bin = "gh"
	}
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, ghCITimeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, bin, args...)
		cmd.Dir = dir
		return cmd.Output()
	}
}

// checkRuns mirrors GitHub's REST check-runs response (the fields we read).
// A null conclusion (run not finished) unmarshals as "".
type checkRuns struct {
	TotalCount int `json:"total_count"`
	CheckRuns  []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

// failureIsh reports whether a check-run conclusion blocks the branch.
// neutral/skipped are non-blocking by design (GitHub's own merge-gate reading).
func failureIsh(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "cancelled", "action_required":
		return true
	}
	return false
}

// greenConclusion is the ALLOWLIST of passing conclusions. Anything a completed
// run reports that is neither this nor failureIsh (e.g. GitHub's `stale`, or any
// conclusion GitHub adds later) is treated as not-yet-passing — evidence must
// fail toward "pending", never default to green on an unrecognized value.
func greenConclusion(conclusion string) bool {
	switch conclusion {
	case "success", "neutral", "skipped":
		return true
	}
	return false
}

// pollCICheckRuns is verification leg 1 (rev7/T3.1): for every
// completed_candidate worker, query its branch's GitHub check-runs and record
// the outcome as ledger evidence:
//
//   - all green → ONE verification_artifact event. Evidence for the human, NOT
//     verification: the worker stays completed_candidate and Verify (the human
//     diff-gate) remains the only path to completed_verified.
//   - any failure-ish conclusion → ONE pending confirm escalation.
//   - checks still running / not yet created → nothing; re-poll next sweep.
//   - runner error or malformed JSON → transient; nothing this sweep.
//
// Best-effort throughout — no CI outcome or error may ever fail the sweep.
func (e *Engine) pollCICheckRuns(ctx context.Context, all []core.Worker) {
	if !e.CI.Enabled || e.CI.Runner == nil {
		return
	}
	for _, w := range all {
		// A candidate with no worktree or head has nothing addressable to poll.
		// head_commit is gated at the intake boundary, but it is interpolated into
		// a `gh api` PATH here — validate again at this exec boundary so a stray
		// `../` head can never traverse to another GitHub API endpoint under the
		// daemon's gh credentials (defense in depth).
		if w.State != core.WorkerCompletedCandidate || w.Worktree == "" || !core.LooksLikeRev(w.HeadCommit) {
			continue
		}
		out, err := e.CI.Runner(ctx, w.Worktree,
			"api", "repos/{owner}/{repo}/commits/"+w.HeadCommit+"/check-runs?per_page=100")
		if err != nil {
			log.Printf("arco: ci: check-runs poll failed for %s: %v", w.ID, err)
			continue
		}
		var runs checkRuns
		if err := json.Unmarshal(out, &runs); err != nil {
			log.Printf("arco: ci: malformed check-runs JSON for %s: %v", w.ID, err)
			continue
		}
		// Zero check-runs is pending-for-safety: checks may simply not have been
		// created yet, so it must never read as success. total_count > the page we
		// got means GitHub paginated (default 30/page) and a failing/pending check
		// could be on an unseen page — also pending, never green on a partial view.
		pending := runs.TotalCount <= 0 || len(runs.CheckRuns) == 0 || runs.TotalCount > len(runs.CheckRuns)
		var failed []string
		for _, r := range runs.CheckRuns {
			if r.Status != "completed" {
				pending = true
				continue
			}
			if failureIsh(r.Conclusion) {
				failed = append(failed, r.Name)
			} else if !greenConclusion(r.Conclusion) {
				pending = true // completed but not a recognized passing conclusion (e.g. stale)
			}
		}
		switch {
		case len(failed) > 0: // a failed check escalates even while others still run
			e.ciEscalateFailure(ctx, w, failed)
		case pending:
			// wait — the next sweep re-polls
		default:
			e.ciRecordSuccess(ctx, w, runs)
		}
	}
}

// ciRecordSuccess appends the green-CI verification_artifact event. Idempotency
// is ledger-keyed, not in-memory: AppendEvent dedups on (source,
// source_event_id), so one artifact per (worker, head SHA) holds across sweeps
// AND daemon restarts. Payload is a short check summary — names/conclusions
// only, never task text.
func (e *Engine) ciRecordSuccess(ctx context.Context, w core.Worker, runs checkRuns) {
	checks := make([]string, 0, len(runs.CheckRuns))
	for _, r := range runs.CheckRuns {
		checks = append(checks, r.Name+":"+r.Conclusion)
	}
	payload, _ := json.Marshal(map[string]any{
		"result": "success",
		"head":   w.HeadCommit,
		"total":  runs.TotalCount,
		"checks": checks,
	})
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		_, _, _, err := tx.AppendEvent(core.Event{
			Kind: "verification_artifact", WorkerID: w.ID, SessionID: w.OwnerSession,
			Actor: "reconcile", Source: "internal",
			SourceEventID: "ci:" + w.ID + ":" + w.HeadCommit,
			Payload:       string(payload),
		})
		return err
	})
	if err != nil {
		log.Printf("arco: ci: artifact append failed for %s: %v", w.ID, err)
	}
}

// ciEscalateFailure opens the red-CI confirm escalation. OpenEscalation's
// one-pending-per-worker dedup keeps repeat sweeps from piling escalations up.
func (e *Engine) ciEscalateFailure(ctx context.Context, w core.Worker, failed []string) {
	action := "CI checks failed on the worker's branch"
	detail := "failed checks: " + strings.Join(failed, ", ")
	var opened bool // NEWLY opened (not a dedup) → notify
	err := e.Store.WithTx(ctx, func(tx core.Tx) error {
		// Detect newly-opened for the notify card in the SAME serialized tx
		// (race-free), mirroring checkStall — a dedup must not re-notify.
		pend, err := tx.ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: w.ID})
		if err != nil {
			return err
		}
		if _, err := tx.OpenEscalation(core.Escalation{
			WorkerID: w.ID, SessionID: w.OwnerSession, Kind: "confirm",
			QuestionClass: "proceed-confirmation", ActionClass: core.ClassAmbiguous, Tier: core.TierMedium,
			ActionFingerprint: "ci:" + w.ID + ":" + w.HeadCommit,
			Action:            action, Detail: detail,
		}); err != nil {
			return err
		}
		opened = len(pend) == 0
		return nil
	})
	if err != nil {
		log.Printf("arco: ci: escalation failed for %s: %v", w.ID, err)
		return
	}
	if opened {
		e.notifyCard(w.OwnerSession, notify.FormatEscalation(notify.EscalationCard{
			WorkerID: w.ID,
			TaskTail: taskTail(w.Task),
			Question: action + " — " + detail,
		}))
	}
}
