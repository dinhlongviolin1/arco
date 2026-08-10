# Rev-7 product review package (T4.3)

For the operator (joint review). Assembled 2026-08-10 by the rev-7 factory
orchestrator after all board tasks #1–#21 closed. Main: `fdbf3a4`.

## What shipped (18 feature PRs + docs, #72–#9x, all squash-merged to main)

### M1 — Operator surface & D9 core
- #77 T1.1 ntfy decision cards via shoutrrr — escalation card w/ context + draft + one-tap answer; primary human interface (replaced webhook + Telegram roadmap).
- #74 T1.2 GET /v1/status + `arco status` one-screen fleet view.
- #75 T1.3 stall_n enforced in sweep; dead knobs crash_loop_restarts/max_spawns deleted.
- #73 T1.4 EscalationDTO exposes brain_rationale + draft_confidence (CLI too).
- #78 T1.5 D9 supervision modes auto|assist|manual (session dial, mode matrix).
- #76 T1.6 SO_PEERCRED intake binding.
- #79 T1.7 §E test debt + kill-9 crash matrix (test/killnine_test.go, W1–W4 + idempotence coda).

### M2 — Hardening & brain quality
- #82 T2.1 herdr events.subscribe socket client (D1 push intake).
- #80 T2.2 srt sandbox wrapper at launch seam (opt-in, off by default).
- #83 T2.3 credentials-dir file handoff — kills MED-5 (creds as 0600 files in 0700 dir outside worktree; env carries only CREDENTIALS_DIRECTORY pointer).
- #81 T2.4 budgeted brain context: facts/summary/memory tiers into prompt.
- #84 T2.5 /metrics endpoint (promhttp).

### M3 — Verification, fleet, autonomy
- #86 T3.1 CI check-runs polling → verification_artifact (config-gated `ci_check_runs`; evidence only).
- #89 T3.2 in-daemon merge queue (`merge_queue` knob; event-sourced ledger items, scratch-clone integrate → test gate → push; kickback = confirm escalation; evidence only, never auto-verifies).
- #90 T3.3 cross-VM routing: Engine.VMs registry from [[vms]], per-worker routing of launch/prompt/liveness/heads/kill/diff; per-VM sweep posture; unknown VM refuses, never local fallback; local-only herdrsock/activity guarded against cross-host pane collisions. Remote worktree provisioning documented OUT (NFS constraint, §10).
- #87 T3.4 HKDF per-worker intake keys (arco/intake/v1 info-string); workspace-name fallback removed (403 + audit event); resolve-by-ID before signature verify.
- #91 T3.5 autonomy earn-out per question_class: migration 0007 `draft_agreement` tally written only inside the human decide() tx (questions agree modulo whitespace/case; confirm approval agrees); sweep resolves earned-out drafted QUESTIONS via new `Tx.AnswerQuestionBrain` (answered_by='brain', scope-less → grant-free) only under mode-auto ∧ VerificationLive(ci_check_runs∨merge_queue) ∧ ≥10 decisions ∧ ≥0.9 agreement (knobs; non-positive = never); confirms never auto-answered; every auto-answer appends an `auto_answer` audit event carrying the justifying stats + ntfy card; `arco autonomy` / GET /v1/autonomy report. Note: 0007 also adds a read-only `grants` view aliasing session_grants (operator/audit SQL convenience; no write path).
- #88 T3.6 human-activity back-off: auto→assist demote on human pane activity (self-op window suppression), quiet-period restore; restart fails toward less autonomy.
- #85 T3.7 herdr plugin exposing arco status.

## What moved (design decisions worth flagging)
- Verification is now two-legged EVIDENCE (CI + merge queue) but completed_verified still requires the human diff-gate — deliberate.
- Autonomy is earned, not configured: earn-out promotion requires a live verification leg; T3.6 activity back-off and T3.5 promotion compose (demotion pauses auto-answers via the mode gate).
- Web dashboard killed per plan-rev7 (collie/herdr-remote own visual surfaces); ntfy cards + status CLI + /metrics are the operator surface.
- Cross-VM: routing/admission landed; remote worktree provisioning is explicitly out (shared-storage constraint documented) — a remote-VM spawn is only correct with the worker root on storage shared with that VM.
- Merge queue pushes to the worktree's origin; non-bare targets kick back with the git error (documented §11 note).

## Verification story (T4.2 evidence)
- Full `go test -race ./...` matrix: PASS on main @ce72326 (baseline; rerun on final main). All 25 packages ok.
- Kill-9 crash matrix: in-suite (test/killnine_test.go), part of the matrix run.
- MED-5 closure: verified in code (spawn.go cred handoff block) + spawnenv scrub tests + §12 marked FIXED.
- Live single-VM smoke (§11 rerun, this host, herdr 0.7.5, pool p1/clavis qwen-1, binary from main @2e21293):
  - VERIFIED LIVE: daemon boot + preflight, pool create/list, repo dispatch (clone → quarantine → permcompile → herdr launch), pane created (w3:p1) with per-worker worktree cwd, MED-5 cred FILES on disk (0600 in 0700 creds/, ANTHROPIC_* + intake absent-since-no-master; env carried only the pointer), stall detection fired at stall_n=3 (T1.3) → question escalation → `arco answer` resumed worker AND delivered text to the pane, re-stall re-fired, `arco kill` terminated + RECLAIMED the herdr pane (MED-3), `arco status`/`--json`, GET /v1/workers, /metrics (7 arco_* series), `arco autonomy` + GET /v1/autonomy live (verification_live=false with both legs off — correct; all classes 0/0; the undrafted stall answer correctly fed no tally).
  - **FINDING (open item, documented gap — not a regression): the authenticated-completion leg FAILS post-T2.3.** The worker's Claude TUI sat at "Not logged in": creds now arrive as $CREDENTIALS_DIRECTORY files and §12 explicitly makes CONSUMPTION "a documented contract" (agent/wrapper must source the files itself) — but nothing in the default launch does, and Claude Code doesn't read them natively. §11's procedure as written was live-verified PRE-T2.3 (creds via --env argv); post-T2.3 it launches an unauthenticated agent. Fix options for joint review: (a) launch-seam cred-sourcing shim (sh -c 'export … ; exec claude …'), (b) fold into the T2.2 srt wrapper, (c) settings.json env-block compile after cred resolve. Until one lands, §11 needs an operator-supplied wrapper and the doc should say so loudly.
- Guideline-test discipline held for every task: orchestrator-authored RED tests, tamper-checked at merge (zero diffs across all rev-7 PRs).

## Open questions for joint review
0. **Cred consumption gap (from the live smoke):** post-T2.3, a spawned agent gets cred FILES + $CREDENTIALS_DIRECTORY but nothing sources them into its env — Claude Code launches unauthenticated. Pick a fix: launch-seam shim / srt-wrapper fold-in / post-resolve settings env compile. (Not fixed during the factory run — new scope on an auth-critical path.)
1. Earn-out defaults (10 decisions / 0.9 agreement) — right bar? Per-class overrides wanted?
2. herdr 0.8.0 contract re-verification pending (0.7.5 installed) — upgrade plan?
3. Remote worktree provisioning (the NFS constraint) — build a remote provisioner next rev, or accept shared-storage-only fleets?
4. Merge queue test gate default: ship a recommended merge_queue_test_cmd or leave unset?
5. vms ledger table is dead schema (config is the registry source) — drop it in a future migration?
6. `crash_loop_window` survived T1.3's dead-knob deletion: still parsed + defaulted, read by nothing (T4.1 observation). Delete or implement?
7. `internal/vm/local.go` Prompt still carries the `--` end-of-options TODO (named cross-VM gate; T4.1 observation).

## M4 close-out results (T4.1/T4.2)
- T4.1 (#92): hardening §13 added (10 PR/knob-linked control subsections, grounded in code — e.g. the real 4-action Allows matrix, not the task-table prose); §12 updated in place (MED-5 struck FIXED, inert-knobs rewritten). herdr-contract re-verified read-only at 0.7.5 with REAL drift fixed: Kill is `workspace close` (not `agent send-keys` — retires the C-c open item), AgentInfo field set completed, events.subscribe + agent-manifest contracts documented, dated 10-point pending-0.8.0 re-check table added. Dashboard §2 re-scored conservatively (P2 88→95, full vision 58→72, PASS-2 95→100, PASS-3 55→85, CLI 60→76, cross-VM 40→70, memory 35→65, notifications 5→80; justifications in the commit body).
- T4.2: final uncached `go test -race ./...` on main all-green (24 pkgs, kill-9 matrix in-suite); MED-5 argv closure verified in code; live §11 smoke results above (incl. open question #0).
