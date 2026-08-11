# Review follow-ups (open improvements)

Tracked backlog from the multi-agent code reviews (rev-7 4-agent pass + the
2026-08-11 20-agent fable audit). Each item: what it is, why it matters, and
status. **Fix discipline:** verify against the code, add a regression test,
small PR. Fixed items are struck once merged; keep the reasoning for provenance.

Severity key: **HIGH** = wrong/unsafe behavior in a real deployment ·
**MED** = real gap, bounded blast · **LOW** = polish / defense-in-depth ·
**DOC/TEST** = missing coverage of a load-bearing invariant.

---

## Fixed (merged) — kept for provenance

- ✅ **openai-key redaction missed modern keys** (#102) — regex widened to
  `[A-Za-z0-9_-]`; live key no longer reaches the LLM/ledger.
- ✅ **mergeq test-gate ran worker code with the daemon's full env** (#102) —
  env now scrubbed (P1).
- ✅ **escalation `answer_text` persisted unscrubbed** (#102) — scrubbed at the
  decide() chokepoint.
- ✅ **git calls (GitHeads/Diff) had no timeout** (#102) — bounded via
  `l.bounded()`; a wedged git/ssh no longer hangs the sweep loop.
- ✅ **CI evidence failed open on unknown/paginated conclusions** (#102) —
  allowlist (success/neutral/skipped) + `total_count>page` ⇒ pending;
  `per_page=100`.
- ✅ **WithTx leaked the write-tx on panic** (#102) — deferred rollback.
- ✅ **phase-3 finalize ran on the request ctx → worker wedged `starting`**
  (#103) — finalize now commits on `e.bg()`; regression-tested.
- ✅ **rollup ran on manual sessions / after estop / on waiting-paused parents**
  (this PR) — D9 mode gate + execution-time `Paused()` check + `rollupEligible`
  tightened to running|starting & non-pool; regression-tested.
- ✅ **SSH_AUTH_SOCK/GPG inherited by workers** (this PR) — `spawnenv.ScrubWorker`
  strips SSH agent + GPG keyring from the worker launch env (arco's own git
  subprocesses keep them for ssh:// clones); regression-tested.
- ✅ (earlier batches) observed_head injection gate, mergeq/quarantine git
  hardening, sweep starting-worker skip, AgentKind row, ledger/socket 0600,
  install.sh checksum, dead-config removal, env-denylist expansion, mergeq
  ancestor idempotence, HTTP header/idle timeouts, ErrPaused→503, estop.

---

## Open — HIGH

- **civerify trusts the worker-writable git remote → forge green CI**
  (`civerify.go:93`). `gh api` resolves `{owner}/{repo}` from the worktree's
  live git remote, which the agent can rewrite (`git remote set-url origin
  <attacker>`). Fix: pin `owner/repo` at spawn from the session's repo and pass
  `gh api -R owner/repo …`, never from the worktree remote. (Blast bounded:
  ci_check_runs is opt-in and the human diff-gate is still authoritative, but
  the evidence leg is spoofable.)

## Open — MED

- **rollup never terminates / no rate cap / inert Summary** (`rollup.go`,
  `sweep.go triggerRollups`). Remaining after the safety-gate fix: (a) a parent
  whose child finished long ago still burns one brain call every RollupInterval
  forever — add a "child completed since last rollup" predicate; (b) rollup
  calls bypass the per-session BrainRate cap — count/limit them; (c)
  `Worker.Summary` is never written, so the rollup prompt summarizes empty
  results — wire a summary writer or drop the field + fix the doc; (d)
  `assembleRollupContext` has no size budget (grows with terminal-child count).
- **Delegate DOA on real herdr** (`engine.go launchAndFinalize` via
  `delegate.go`). Brain delegation `Prompt`s a workspace *label*, which herdr
  0.7.5 rejects as a target, so every delegated child fails on a real backend
  (only the Fake accepts it). Also: delegated children skip pool-lease admission
  and every launch guardrail. Wire the child through the real
  workspace-create→agent-start launch (as Spawn does) + lease acquisition.
- **stall detector false-positive on push-updated heads** (`sweep.go checkStall`
  + `engine.go ApplyEvent`). A worker that reports its head via intake between
  sweeps looks "no progress" (row head already == observed head) and gets
  blocked after StallN sweeps though it's actively committing. Fix: baseline
  stall on the previous *sweep's* head (in-memory), or reset stall_count in
  ApplyEvent on headChanged.
- **mergeq hardening** (opt-in): no per-command timeout (a hung clone/fetch/test
  wedges the sweep — add `context.WithTimeout` per integrate); a poison item
  (unreadable worker row) head-of-line-blocks the queue forever (treat as a
  kick/drop, not a retryable error); the ancestor short-circuit mints a
  "merged" artifact for a no-op worker whose head==base (gate on head!=base +
  proper-descendant).
- **AuditDeniedAttempt notification-bombing** (`audit.go`). A worker can post
  unlimited distinct-id `denied_capability` events, each firing an urgent card +
  a new escalation row (alert fatigue + unbounded growth). Require non-empty
  source_event_id at the API; skip re-open+card when a pending danger confirm
  for the same capability already exists.
- **reapEscalations expires by worker-id, not escalation-id** (`sweep.go`). A
  fresh escalation opened between the pending snapshot and the per-worker expire
  tx can be expired at age ~0 and the worker paused. Fix: expire
  `WHERE id=? AND status='pending'` (an `ExpireEscalation(id)` sibling). Also
  closes the workerless-escalation never-expires latent case.
- **sandbox preflight false-assurance** (`preflight.go`, `config.go`,
  `vm/sandbox_wrap.go`). Enabling `[sandbox]` silences the "workers not isolated"
  warning while confining nothing (SandboxWrap is unwired — herdr owns the
  command). Fix the config doc-comment + make `sandbox_enabled` stay warning
  until an srt-wrapped herdr manifest is actually in effect.
- **event dedup identity is global, not per-worker** (`ledger/tx.go`,
  `0001_init.sql`, intake `source` defaults to `"herdr"`). If herdr's
  `source_event_id` is only per-worker-unique, worker A's id can dedup/suppress
  worker B's real liveness event. Fold the resolved `worker_id` into the dedup
  key (`UNIQUE(source, worker_id, source_event_id)` via a new migration, or the
  check-side equivalent).

## Open — LOW / DOC / TEST

- **`agent prompt` still lacks a `--` end-of-options guard**; the live
  Dispatch path (`engine.go`) passes raw task text — a task starting with `-`
  is flag-parsed. Add `--` before positionals (or use the socket API).
- **PromptReady double-submit guard has ZERO tests** (`vm/local.go`) — the
  single most consequential untested invariant (a regression double-runs or
  drops a worker's task). Add fakeHerdr stall/status tests.
- **cappedPatch swallows all errors** (`vm/local.go`) — returns an empty/partial
  patch as complete with `Truncated=false`; a verify reviewer sees "no changes".
- **PromptReady self-op window (5s) < its own retry schedule (~30s)**
  (`activity.go`) — a retried delivery's echo can demote the session it just
  prompted (auto→assist under nobody). Floor the window at the retry span.
- **activity restore can override an operator's explicit assist** across mode
  flips (`activity.go`) — check the last mode_change actor before promoting.
- **herdrsock**: one vanished pane drops per-pane subscriptions for the whole
  batch until reconnect; a non-bare-entry subscribe rejection leaves a zombie
  session; fixed 1s reconnect backoff (no jitter/exponential). All sweep-covered
  (push is advisory), but degrade silently.
- **config**: `os.Stat` gate silently drops an explicitly-passed `--config` on a
  stat error (EACCES/ENOTDIR); no positivity validation on duration knobs
  (`sweep_interval = 0` panics `time.NewTicker`); unknown/typo'd keys silently
  no-op. Add a `validate()` pass and reject-or-log unknown keys.
- **daemon**: sweep errors are swallowed (no log/metric) — a herdr outage makes
  the loop silently no-op; boot aborts entirely if herdr is down at start
  (routing off); serial pre-listen VM probes delay the socket on an unreachable
  fleet.
- **misc**: `%q`-built event payloads can be invalid JSON (control/non-BMP
  bytes); tokenized repo URLs persist verbatim + echo on failure; `MissThreshold
  <= 0` has no floor; `errStatus` maps lease/VM-capacity refusals to 500 (should
  be 409/503); CLI/client have no HTTP timeout (hangs on a wedged daemon).

## Standing architectural decisions (deployment, not code bugs)

- **Worker isolation / unauthenticated control routes.** Workers run as the
  daemon's UID and the mutating control routes trust the 0700 socket dir, so a
  compromised worker can reach them and read sibling cred files. The real fix is
  a **separate worker UID + the srt sandbox** — an OS/deployment change. Fine
  for the homelab-behind-Tailscale trust model; required before untrusted or
  production use.
- **Cred-consumption gap.** Spawned agents get `$CREDENTIALS_DIRECTORY` cred
  files but nothing sources them into the agent's env, so autonomous completion
  doesn't work end-to-end (supervision/estop/dispatch/kill do). Needs a
  launch-seam sourcing shim, an srt-wrapper fold-in, or a post-resolve settings
  env compile.
