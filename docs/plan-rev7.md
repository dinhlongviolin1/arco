# arco — Plan rev-7

Status: **proposed** (supersedes the informal "rev-7 candidates"; does not unfreeze build-guide-rev6 §B/§C — schema and contracts stay frozen).
Date: 2026-08-07.
Inputs: 7-agent deep review, 3-agent research wave, and a 3-round consideration pass (herdr 0.8.0 source read, ~200-plugin ecosystem sweep, red-team, source audit of competitor `0xGosu/herdr-auto-pilot`).

---

## §0 Repositioning

**The goal, in the owner's own words:** a support tool. Drive agents manually in herdr on the laptop when home; while away, supervise the 3-VM Tailscale homelab from the phone; eventually the agents earn enough trust that manual driving becomes "peek in when curious."

**The one-liner (new):**

> hap answers your agents' prompts. Auto Mode approves their tool calls. Collie extends your thumb.
> **arco is what tells you the work actually got done — on every machine, with receipts.**

**Why the reposition.** The 2026 landscape has three layers:

| Layer | Who lives there | Status |
|---|---|---|
| 1. Remote control (your thumb, extended) | collie, herdr-remote, Anthropic Remote Control, Codex Remote | crowded, first-party |
| 2. Prompt answering (unblock the agent) | herdr-auto-pilot ("hap"), Claude Code Auto Mode | taken — hap is good, Auto Mode is first-party |
| 3. Outcome supervision (prove the work, keep receipts, across the fleet) | **nobody** | arco's slot |

Layer 3 *gains* value as layers 1–2 improve: the more prompts get auto-answered, the more unwatched work exists that needs verification and an audit trail. This is the pillar that survived every red-team attack.

**What arco explicitly does NOT compete on anymore:**
- Manual remote driving (collie / Remote Control own it).
- Prompt-answering maturity (hap's loop is a month ahead and it's their whole product; Auto Mode does permissions first-party on better signals than anyone can scrape).

---

## §1 Competitive corrections (verified)

1. **`0xGosu/herdr-auto-pilot` ("hap")** — MIT, solo (Vincent Tran), v0.5.31 (2026-08-04). Source-audited. Has: shadow learning with a real draft-vs-human agreement metric, exact-hash→BM25→local-embedding rule matching (bleve + llama-go), CI-validated never-auto regex corpus, dual runaway guard, rich decision audit log, BYO-CLI LLM via a stdio MCP server (`get_context`/`submit_decision`). **Lacks (source-verified):** any work-product verification (its `verifyunblock` only re-checks `agent_status` ~1s after send), phone escalation (desktop toast + own TUI only), multi-host (one local socket), and the result leg of the ledger (decision→delivery only, never decision→outcome).
2. **Anthropic Auto Mode** (GA 2026-07) + **Remote Control** (2026-02): first-party permission-prompt adjudication and phone driving — per session, Claude-only, cloud-mediated. No fleet, no verification, no ledger.
3. **Gas City** (Gas Town successor) ships a herdr session-provider backend — but it is a whole factory lifestyle, not a supervisor of your existing sessions. Its Witness/Deacon supervision is notoriously unreliable per Yegge himself.
4. **herdr HEAD (0.8.0) facts:** Claude state hooks were REMOVED in 0.6.7 (stale-authority bugs) — Claude status is regex screen-scraping of pane text; the socket API (`events.subscribe`, NDJSON over Unix socket) is the documented push channel, protocol-versioned; docs explicitly position the socket API as the substrate for external orchestrators; zero signs of native supervision plans. The `--` task-loss parse looks fixed at HEAD (0.8.0 #1878 suggests the symptom was prompt-send timing).

---

## §2 Decisions (D1–D9)

Legend: **CHANGED** = differs from the rev-7-candidates draft. **NEW** = didn't exist. **KEPT** = unchanged.

### D1 — Signals: subscribe to herdr; own hooks deferred — CHANGED
- **Was:** wire Claude Code Stop/UserPromptSubmit/SessionStart hooks into arco in H2 (spike S2).
- **Now:** consume herdr's documented `events.subscribe` (push, per-pane filterable) + `agent.list` resync on reconnect, replacing the polling input. arco's own Claude hooks are **deferred** until autonomy earn-out concretely needs authoritative turn boundaries — and even then, hooks are a fusion *signal*, never authority (herdr already learned that lesson the hard way in 0.6.7; arco's ledger-reconcile-is-king architecture agrees).
- Plain words: herdr will tap arco on the shoulder when an agent's state changes, instead of arco asking every 30 seconds. We don't build our own Claude wiretap until we actually need it.

### D2 — Sandbox: adopt srt; drop bespoke bwrap+proxy — CHANGED
- **Was:** build a bubblewrap + go-landlock wrapper and an egress allowlist proxy.
- **Now:** wrap agent launches with **anthropic-experimental/sandbox-runtime (`srt`)** — it *is* the planned bwrap+egress-proxy, already written and vendor-maintained (Node dep acceptable; Claude Code needs Node anyway). Layer 2: `systemd-run --user` transient units (`ProtectHome=`, `ReadWritePaths=`, `IPAddressDeny=`) for cgroup/IP isolation with zero new deps. Skip firejail/gVisor/raw-bwrap-ownership.
- Plain words: Anthropic already wrote the cage we were going to build. Use theirs; add systemd's free locks as a second layer.

### D3 — Intake identity — KEPT
- Local: bind events to worker UID via `SO_PEERCRED` (H1). Network (cross-VM): HKDF per-worker derived HMAC keys + replay guards (H3). Remove the workspace-name fallback (`server.go:508-528`) that lets any key holder forge any worker's events.

### D4 — Verification is the moat — PROMOTED
- **Was:** merge queue as one hardening item among eight.
- **Now:** the differentiating layer, phased: (a) diff-gated verify (already live) stays the floor; (b) **test leg reuses the repo's existing CI** (read check-runs) rather than running tests in-daemon; (c) in-daemon serialized rebase→test→ff-merge FIFO with conflict kickback lands in H3, emitting a `verification_artifact` event that upgrades diff-gated to proof-gated. Nothing self-hostable and light exists to buy (verified: bors-ng dead, GitHub MQ needs org repos, Aviator is enterprise-weight).
- Plain words: "the agent says it's done" becomes "the branch rebases, tests pass, and here's the artifact that proves it." This is the thing nobody else on herdr does.

### D5 — Brain: classifier + shadow drafter; answering ambition demoted — CHANGED
- **Was:** brain quality push toward auto-answering.
- **Now:** brain = escalation **classifier + shadow drafter** only, indefinitely. Fix context starvation (feed Facts/ContextSummary/memory into the prompt — `brain_apply.go:136-167`); enforce `stall_n`; expose `brain_rationale` + `draft_confidence` in EscalationDTO. **Adopt hap's LLM-integration pattern**: shell out to a configured agent CLI (`claude -p`) attaching a stdio MCP server exposing `get_context`/`submit_decision` — structured decisions back, no provider SDK. Earn-out (shadow→auto per question_class by measured agreement) is **gated on D4 being live**: autonomy without verification is how unwatched agents burn days.
- Plain words: the brain suggests, you decide — until the receipts system is good enough to catch its mistakes. And we borrow the competitor's cleverest plumbing (it's MIT).

### D6 — Operator surface: phone decision cards ARE the product — CHANGED
- **Was:** `arco status` / logs / webhook as operator niceties; Telegram + web dashboard as roadmap add-ons.
- **Now:** **ntfy push decision cards are the primary human interface** (escalation card: context + draft + one-tap answer/confirm; completion receipts). Implementation: `shoutrrr` (nicholas-fedor fork) as the lib → self-hosted `ntfy` (iOS/Android push, UnifiedPush). This deletes bespoke webhook code AND the Telegram roadmap item. `arco status` CLI table stays (~200 lines); 50-line `promhttp` `/metrics` as a side door; **web dashboard is killed** — collie/herdr-remote own visual remote surfaces, and at most arco ships a thin herdr plugin exposing status.
- Plain words: when you're away, arco's whole face is a phone notification you can answer with one tap. At home, its face is herdr itself plus one CLI command.

### D7 — Secrets: systemd-creds, not a product — CHANGED
- **Was:** bespoke 0600 tmpfs file now, credential-helper-over-socket later.
- **Now:** arco's own secrets (HMAC intake keys, provider keys, ntfy tokens) via **`LoadCredential=`/`systemd-creds`** — files in `/run/credentials/arco.service/`, never argv/env, encrypted at rest, TPM-bindable, ~0 lines of Go. Agent handoff keeps the per-spawn tmpfs-file design (it *is* the systemd credential model), sourced from the credentials dir. Fixes MED-5 (`--env` argv leak) at the root. OpenBao/Infisical: verified alive, rejected as ops burden. `age` stays for at-rest blobs.
- Plain words: yes we need a secrets manager — and systemd already shipped one. No new daemon to babysit.

### D8 — Debt & docs — KEPT (additions)
- Three missing §E tests; kill-9 crash matrix; delete-or-implement dead knobs (`crash_loop_restarts`, `max_spawns`, `stall_n` — D5 implements stall_n, delete the rest or wire them); quota-policy contingency. Additions: competitive teardown updated for **hap + Gas City + Auto Mode** (replaces the Gas Town gap); adopt hap's idea of a **CI-validated never-auto corpus** for whatever safety regexes arco keeps.

### D9 — Coexistence: supervision modes — NEW
- Per-session mode: **`auto | assist | manual`**. `manual`: arco observes + ledgers, zero actions, zero pings. `assist`: notify + draft, never act. `auto`: earned (D5 gate).
- **Auto back-off:** composite "human active in last N minutes" timer from three source-verified proxies: (a) `workspace/tab/pane.focused` events arco did not itself cause, (b) `Done→Idle` agent-status transitions arco didn't trigger (that flip is human-focus-driven in herdr), (c) `pane.scroll_changed` with `offset_from_bottom` movement (only humans browse scrollback). Any of these → drop `auto`→`assist` for the session.
- **Hard rules:** arco never calls `pane.focus`/`agent.focus` in auto mode (it fakes herdr's seen flag); arco's `send_text` is indistinguishable from human typing at the PTY, so the **ledger is the only record of who answered** — every arco-sent answer must carry an actor field.
- Plain words: when you're at the keyboard, arco automatically gets out of your way — and you can always tell, from the ledger, which answers were yours and which were arco's.

---

## §3 Cut list (out of scope for rev-7)

| Cut | Why |
|---|---|
| Web dashboard, `attach`, session detail UI | collie / herdr-remote / herdr itself own visual surfaces |
| Session trees, supersession, depth-2 | speculative scaffolding for a fleet size that doesn't exist |
| Memory tiers 3–4 (revisions, playbooks) | tiers 1–2 (facts, context summary) suffice for the brain fix |
| Provider pools beyond one static pool | single-operator homelab; leases machinery already built stays |
| Telegram forum-topic UX | replaced by ntfy cards (D6) |
| Bespoke bwrap/egress-proxy ownership | replaced by srt (D2) |
| Own Claude hook layer (for now) | replaced by herdr events.subscribe (D1); revisit at earn-out |
| P3 "swarm" ambition | demoted to "3 known VMs over Tailscale" — SSH layer (#69/#70) covers it |

Roughly a third of rev-6's remaining surface. The frozen §B schema keeps unused tables (playbooks, memory_revisions) — harmless, no migration needed.

---

## §4 Phases

### H1 — Trust & floor (pure-add, ~days)
1. ntfy decision cards via shoutrrr (escalations + completion receipts) — **the product**
2. D9 supervision modes + human-activity back-off timer
3. `arco status` CLI table
4. Enforce `stall_n`; delete remaining dead knobs
5. `brain_rationale` + `draft_confidence` in EscalationDTO
6. `SO_PEERCRED` local intake binding
7. Missing §E tests + kill-9 matrix

### H2 — Signals & containment (medium risk)
1. `events.subscribe` socket client (replace poll input; resync on reconnect)
2. srt sandbox wrapping at the launch seam (`local.go:387`) + systemd-run layer 2
3. systemd-creds migration (fixes MED-5 at root)
4. Brain context fix (facts/summary/memory) + MCP `get_context`/`submit_decision` pattern
5. 50-line `/metrics`

### H3 — Proof & fleet (the moat)
1. Merge queue FIFO + `verification_artifact` (proof-gated verify)
2. Cross-VM wiring of the validated SSH layer (3-VM topology)
3. HKDF per-worker keys; remove workspace-name fallback
4. Earn-out per question_class — gated on H3.1 live
5. Thin herdr plugin exposing `arco status` (ecosystem citizenship)

---

## §5 Open calls (owner decision needed)

1. **hap relationship**: ignore, borrow patterns only (current plan), or reach out — arco-supervises-outcomes-while-hap-answers-prompts is a genuinely complementary split, and both are herdr-citizen Go daemons.
2. **Merge queue timing**: H3 as planned, or pull the CI-check-run leg (D4b) into H2 since verification is now the moat?
3. **Earn-out thresholds**: config-tunable or frozen like §C pinned defaults?
4. **Upstream PRs to herdr**: which first — env-file secrets mechanism, or typed blocked-reason in `agent list`?

---

## §6 Evidence trail

- herdr source (HEAD 0.8.0, cloned locally): detection manifests `src/detect/manifests/claude.toml`; hook removals `src/integration/claude_settings.rs:22-59`; events API `src/api/schema/events.rs`; seen-flag mechanics `src/app/api_helpers.rs:99-110`, `src/app/runtime.rs:236-244`; multi-client `src/server/clients.rs`.
- hap source (v0.5.31, audited at /tmp/hap-audit): `internal/verifyunblock/`, `internal/classify/classify.go`, `internal/llm/mcpserver.go`, `domain/safety.go`, signatures/decisions schema `store.go`.
- arco findings: `brain_apply.go:136-167` (context starvation), `server.go:508-528` (workspace fallback), deployment-hardening §11 (live loop), §12 (limitations).
- Web-verified 2026: Auto Mode GA, Remote Control, Codex Remote, OpenBao v2.5, shoutrrr fork, ntfy, srt, River-SQLite preview, merge-queue landscape.
