# Manager Harness Blueprint — "how others do it" → what we build

**Status:** In progress (assembled overnight 2026-08-04 while Long slept, via /loop).
**Goal (Long's directive):** "Evaluate how others do it and bring it to us. We can build it, but the harness needs to be top-tier as well as the management of the main agent."
**Two hard bars:** (1) a **top-tier harness**; (2) excellent **management of the main agent** — keeping the primary/manager agent coherent, cheap, and alive over days/weeks.

Companion docs: `HANDOFF.md` (canonical resume point) and `2026-08-03-herdr-manager-design.md` (earlier design). Memory: `~/.claude/projects/-home-dinhlongviolin1-main-coding/memory/herdr-manager-design.md`.

---

## Part 0 — Why we're building a harness at all (decision context)

Adversarial research (4 agents, 2026-08-04) overturned the earlier "pick a brain: Hermes vs claude-as-manager" binary. Key conclusions:

- **The manager brain is only ~15–20% of the decision.** Real bottlenecks bind first and are brain-independent: worker **API rate limits** (~25–50 *actively working* agents per org), then **per-box RAM/CPU** (~50–100 Claude Code processes/box), then a **cross-VM aggregator** you must build regardless (herdr `--remote` tunnels only the TUI, not events).
- **Running any LLM as an always-on "brain in a pane" is the wrong shape.** Claude Code is a Node process with credible heap-leak/OOM reports over long idle runs; its Channels event model is turn-batched, no-ack, session-scoped, 7-day-expiring, and needs a `--dangerously-load-development-channels` flag with no headless support.
- **Right architecture (converged by 3/4 agents):** a **small durable daemon we own** (Go, in/near clavis) that subscribes to herdr's plugin-hook events, keeps a **durable SQLite worker-state ledger**, exposes Telegram, aggregates across VMs — and invokes the LLM **as a short-lived per-event call** (swappable: claude headless / cheap model / Hermes). Short-lived calls can't leak over days.

So the "harness" we must make top-tier = **(daemon + ledger + event bus + messaging) × (excellent main-agent context/memory management for each brain invocation)**. Parts 1–2 below mine the best existing systems for exactly these mechanisms.

---

## Part 1 — How the best systems manage the MAIN AGENT (external teardown) ✅ COMPLETE

Mechanism-level teardown across the canonical references. Sources traced to primaries where noted; softer claims flagged.

### 1. Letta / MemGPT — the "LLM-as-OS / virtual memory" reference (paper arxiv 2310.08560; docs.letta.com)
- **Main context vs external context**, modeled on OS virtual-memory **paging**. Main context = 3 regions: (a) read-only **system instructions**, (b) **core memory** — fixed-size, agent-editable-via-tools text blocks, always in-prompt (block length cap bounds tokens; ~2000-char default *unconfirmed*), (c) **FIFO message queue** holding a recursive summary of evicted messages.
- **External context** = **recall storage** (full raw message log, kept forever) + **archival storage** (arbitrary text, vector-searchable).
- **Memory-pressure eviction loop (the crown jewel):** **70%** of window → inject a "memory pressure" warning so the agent can save what matters *before* loss; **100%** → flush **50%** of the window and regenerate the recursive summary. Nothing is deleted — evicted messages stay in recall, retrievable by tool.
- **Re-injection is dual:** core blocks recompiled into the system prompt **every step** (always-hot, no search); archival/history only via explicit tool calls (`archival_memory_search`, `conversation_search[_date]`, `core_memory_append/replace`). The LLM *reasons about what to remember*.
- **Survives restart:** all state DB-persisted (Postgres+pgvector for archival; SQLite local). Core reloads into prompt; archival/recall stay queryable.
- **Anti-degradation:** nothing truly lost (lossy summary is *recoverable* via search) + 70% agency checkpoint + always-hot identity blocks. **Heartbeats** (`request_heartbeat`) chain autonomous multi-step tool sequences between user turns.

### 2. Anthropic — "context engineering" (anthropic.com/engineering; platform.claude.com/docs)
- Framing: context = finite **attention budget**; **"context rot"** = recall degrades as tokens grow (n² attention + short-seq training skew) — a gradient, not a cliff. Aim for "smallest set of high-signal tokens."
- **Context editing** (server-side): auto-clears stale tool results at **100K-token** trigger, keeps **3 most recent** tool-use/result pairs, placeholders the rest. Reported **84% token cut** on a 100-turn test (*internal, unreplicated*).
- **Compaction** ("primary strategy"): at **150K-token** trigger, summarize → `compaction` block → drop prior blocks. Claude Code variant preserves "architectural decisions, unresolved bugs, implementation details" + carries the **5 most recently accessed files**.
- **Memory tool** (client-side, file-based under `/memories`; you execute the ops): `view/create/str_replace/insert/delete/rename`. Re-enters context only when the agent issues `view`. Injected instruction: *"ASSUME INTERRUPTION: your context window might be reset at any moment."* Memory + context-editing together: **+39%**.
- **Anti-degradation trio:** compaction + **structured note-taking to a file** (the "Claude plays Pokémon" tallies/maps across thousands of steps) + **subagent context isolation** (subagents burn tens of K tokens, return only **1–2K-token** distilled summaries). Plus **just-in-time retrieval** (hold identifiers/paths, load at runtime).

### 3. Cognition (Devin) — "Don't Build Multi-Agents"
- **P1:** share context AND full agent traces, not just messages. **P2:** actions carry implicit decisions; conflicting decisions → bad results.
- Default to a **single-threaded linear agent** (continuous context, no conflicting hidden assumptions). When it outgrows the window, don't split — use **a dedicated (fine-tuned) LLM that compresses history into key details/events/decisions.** Only pushes the ceiling out; "hard to get right."

### 4. Manus — "Context Engineering: Lessons from Building Manus"
1. **Design around the KV-cache** — "single most important metric." Stable prompt prefix (a 1-token diff invalidates cache downstream → **no per-second timestamps**), append-only, deterministic serialization. Cited Sonnet **cached 0.30 vs uncached 3.00 USD/MTok (10×)**; input:output ≈ **100:1**.
2. **Mask, don't remove** tools (removing defs invalidates cache + orphans refs); constrain via logit-masking + consistent tool-name prefixes (`browser_`, `shell_`) to toggle groups.
3. **File system as context** — externalized, unlimited, **always-restorable** compression (drop content, keep URL/path).
4. **Recitation** — a **todo.md** rewritten each step pushes goals to the context tail, fighting "lost-in-the-middle" drift (~50 tool calls/task).
5. **Keep the wrong stuff in** — leave failed actions + stack traces so the model stops repeating mistakes.
6. **Don't get few-shotted** — vary structure to avoid rhythmic ruts.

### 5. Others with distinct mechanisms
- **LangGraph:** **checkpoint = full graph-state snapshot every super-step**, keyed by `thread_id`, to a swappable durable backend (Postgres/SQLite survive restart; MemorySaver doesn't). Enables continuity, **time-travel/replay/fork**, and **node-level fault-tolerant resume** (only the failed node re-runs). Separate namespaced vector **Store** + **LangMem** (hot-path tools vs background extraction). Checkpoints accumulate → prune.
- **Cline / Roo "Memory Bank":** fixed markdown set (`projectbrief`→`productContext`/`systemPatterns`/`techContext`→`activeContext`/`progress`) on an explicit "memory resets between sessions" assumption: *"read ALL memory bank files at the start of EVERY task."* Whole-file reconstruction; churn confined to `activeContext`/`progress`. Roo adds mode-scoped write permissions via MCP (*community, unverified*).

### Ranked STEALABLE mechanisms for a long-lived manager agent
1. **Always-in-context, agent-editable core-memory blocks** — pin identity, mandate, active-worker roster, standing constraints; never page out. *MemGPT. Proven.* Best defense against "manager forgets who it is."
2. **Restorable / reference-based compression** — evict payloads, keep pointers (worker ID, file path, PR URL, log loc). *Manus + MemGPT. Proven.* Antidote to lossy compaction.
3. **Externalized durable memory the agent self-edits via tools** — `/memories`-style store / memory-bank. *MemGPT + Anthropic + Cline. Proven.*
4. **Two-trigger memory-pressure loop (warn @~70% before evict @100%)** — give the agent an agency checkpoint, not silent truncation. *MemGPT. Proven & unique.*
5. **Recitation of the plan/worker-status into recent attention** — live TODO/roster file rewritten each cycle. *Manus + Anthropic. Proven.* Potent for a supervisor of many workstreams.
6. **Structured progress log surviving resets** — init writes checklist; each session reads first, updates at end, "complete only after verification." *Anthropic + Cline. Proven.*
7. **Subagent context isolation with condensed returns** — workers burn tokens; manager ingests only 1–2K summaries. *Anthropic. Proven.* How a manager scales to many workers.
8. **Compaction that preserves decisions, not transcripts** — keep decisions/open threads/recent artifacts, drop redundant tool output. *Anthropic + Cognition. Proven.* Pair with #3.
9. **Keep failures in context** — retain errors/stack traces so the manager stops re-dispatching broken instructions. *Manus. Proven, under-adopted.*
10. **Single continuous thread + full-trace sharing by default; compress before you fork.** *Cognition. Proven stance.*
11. **KV-cache-stable prompt construction** — stable prefix, no volatile timestamps, append-only, mask-don't-remove tools. Compounding cost/latency win for a days-long agent. *Manus. Proven.*
12. **Durable per-step checkpointing with replay/fork + fault-tolerant resume** — snapshot manager state each step to Postgres/SQLite; resume after crash, rewind, or fork. *LangGraph. Durability proven; fork more experimental.* Makes "survives restart across weeks" real.

**Cross-cutting rules:** (a) **Hybrid re-injection** — always-hot blocks for identity/mandate + search-based paging for the long tail. (b) **Assume interruption** — context window is volatile; the external store is ground truth; rebuild coherence from disk, not luck.

**Weak-evidence flags to re-verify:** Letta 2000-char block default + pgvector/HNSW specifics; Anthropic 84/29/39% deltas (internal); Roo real-time-sync/mode-perms (community).

---

## Part 2 — Hermes source teardown 🔶 IN PROGRESS (subagents kept hitting 429; now reading source directly in main loop)

### 2A. Context-engine architecture — `agent/context_engine.py` ✅ (read in full, 2026-08-04)

Hermes's context management is a **pluggable engine behind an ABC** (`ContextEngine`), config-selected via `context.engine` (default `"compressor"`; alternatives like an "LCM" engine or third-party plugins drop into `plugins/context_engine/<name>/`). Only one active. This is a clean, stealable seam — the whole context strategy is swappable without touching the loop. Key design elements:

- **Two ORTHOGONAL verbs, explicitly separated** (this is the standout idea):
  - `compress(messages, ...)` — context too long → make it shorter (summarize/DAG/etc.). Returns a new valid message list.
  - `select_context(request_messages, ...)` — "this turn belongs to a *different* context → use that one instead" (retrieval, topic routing, branch switching). **Request-only**: replaces the messages sent for this one call, MUST NOT mutate persisted transcript. Removes the common anti-pattern of forcing `should_compress()=True` just to get a per-turn callback. (context_engine.py:215-279)
- **`prune_tool_results_only(messages)` — deterministic, NO LLM call** — trims old tool-result payloads on a *low, cost-oriented trigger independent of `should_compress`*, so a large-window engine reclaims re-sent tool output long before full compaction fires. (context_engine.py:194-211) This is Hermes's version of Anthropic "context editing" / Manus "restorable compression" — a cheap prune before the expensive summarize.
- **Compaction params (defaults):** `threshold_percent = 0.75` (fire at 75% of context), `protect_first_n = 3` (head messages verbatim, **plus** the system prompt always implicitly protected), `protect_last_n = 6` (tail verbatim). Per-model overrides via `resolve_model_threshold()` (longest-substring match); `update_model()` recomputes `threshold_tokens` on model switch/fallback. (context_engine.py:121-123, 458-489)
- **KV/prompt-cache is a first-class invariant.** `update_from_response(usage)` ingests canonical buckets including **`cache_read_tokens` / `cache_write_tokens`** — the engine *knows* cache economics per turn. `select_context`'s docstring states prompt-cache stability is "an AGENTS.md invariant": the default no-op leaves the request **byte-identical** so cache is preserved, and cache-control breakpoints are re-derived on the selected list. This is Manus's KV-cache principle baked into the architecture. (context_engine.py:133-143, 253-266)
- **Anti-silent-failure:** `should_compress_info()` returns `(bool, reason)` so when compaction is *skipped* (summary-LLM cooldown, anti-thrash guard) the user is warned instead of silently overflowing to the hard limit. (context_engine.py:149-160)
- **Preflight:** `should_compress_preflight(messages)` = cheap rough check before the API call (no real token count yet); `should_defer_preflight_to_real_usage()` avoids re-compacting from noisy estimates after a compressed request already fit. (context_engine.py:332-347)
- **Post-turn observation hook:** `on_turn_complete(messages, usage, ...)` — ingest/index/summarize what actually happened so the *next* `select_context()` can act on it. Complements selection (before) vs observation (after). kwargs carry turn_id/task_id/api_call_count/interrupted/failed/turn_exit_reason. (context_engine.py:281-328)
- **Clean session lifecycle:** `on_session_start` (load persisted DAG/state), `on_session_end` (real boundaries only — CLI exit / `/reset` / gateway expiry, NOT per-turn), `on_session_reset` (`/new`). (context_engine.py:385-409)
- **Memory-context egress sanitization:** `sanitize_memory_context()` redacts secrets + URL creds (`force=True`) and caps memory-provider text at **6000 chars** (head 4000 + tail 1500 + truncation marker) before it reaches the LLM. (context_engine.py:34-53)

**STEAL for our harness:** (a) the **pluggable context-engine ABC** as the seam for main-agent context strategy; (b) **compress vs select_context** separation; (c) **deterministic no-LLM tool-result prune** on a cheap trigger; (d) **cache-read/write token accounting per turn** to drive cost-aware decisions; (e) **byte-identical default = cache-preserving** invariant; (f) **(bool, reason) skip surfacing** so overflow is never silent; (g) **secret redaction at the LLM egress boundary**.

### 2B. Runaway-loop bound — `agent/iteration_budget.py` ✅ (read in full)

Thread-safe consume/refund counter per agent. **Parent cap = `max_iterations` (default 500); each subagent gets an INDEPENDENT cap = `delegation.max_iterations` (default 50)** — so total iterations across parent+children can exceed the parent cap by design. `execute_code` (programmatic tool-calling) iterations are **refunded** so they don't eat the budget. (iteration_budget.py:17-59) **STEAL:** a hard per-agent iteration budget with independent child budgets is exactly what bounds a manager that dispatches many workers; refunding "cheap" internal turns is a nice touch.

### 2C. Compaction internals — `agent/conversation_compression.py` ✅ (grep-level)
- **Separate cheap "auxiliary" summary model.** Compaction is done by an aux model, not the main model. If the aux's context window is smaller than the main model's compression threshold, Hermes **auto-lowers the live session threshold** (floor ~64K) so a full threshold-sized window can still be summarized, and keeps `threshold_percent`/`tail_token_budget`/summary target in sync. (conversation_compression.py:1577-1723) — i.e. the harness gracefully adapts when the summarizer is weaker than the worker model.
- **`tail_token_budget` derived from the trigger threshold**; **`summary_target_ratio`** sets summary size = threshold × ratio.
- **Idle compaction:** compacts proactively during idle time (status template uses `idle_seconds=3600` — compact after ~1h idle) so the next active turn starts lean. (conversation_compression.py:173)
- **`_verify_compaction_cleared_threshold`** — after compaction, verifies tokens actually dropped below threshold; retry-tokens status (e.g. 250K→120K) surfaces the before/after. (conversation_compression.py:176-256)
- **Anti-silent-overflow warning** (#16775 class): if context is over threshold but compaction can't run, the user is explicitly warned rather than silently overflowing. (conversation_compression.py:151-160)
**STEAL:** cheap-aux-summary-model with graceful threshold auto-lowering; **idle-time proactive compaction**; **post-compaction verification** (don't trust the summarizer blindly); loud-not-silent overflow.

### 2D. Durable memory + self-improving skills — `hermes_state_schema.py`, `memory_manager.py`, `curator.py` ✅ (grep-level)
**Durable state (SQLite, FTS5-backed):** tables include `gateway_routing` (session routing), `session_model_usage` (per-session/per-model token accounting = built-in cost tracking, indexed by session+model), and `messages` (transcript, indexed by platform_msg_id, FTS5-searchable). Migrations follow the Beets/sqlite-utils pattern. (hermes_state_schema.py:433-613)

**Memory tiers (two re-injection paths, confirmed):** `build_system_prompt()` concatenates each provider's whole-file `system_prompt_block()` (always-hot: MEMORY.md/USER.md) + `prefetch_all(query, session_id)` does query-driven retrieval via `_prefetch_provider` (builtin runs synchronously, external providers threaded with timeout so a slow one can't stall the turn). (memory_manager.py:486-547)

**Skill-curation state machine (the compounding "grows with you" loop) — `curator.py` (2019 lines):**
- Skills move through **ACTIVE → STALE → ARCHIVED** by an inactivity walk: `stale_cutoff = now − stale_after_days` (config `stale_after_days`); last-use anchor vs cutoff decides transitions. (curator.py:306-378)
- **Drift/erosion controls (the important part):** **pinned skills never auto-transition** (curator.py:331, and the LLM-curator prompt is told "DO NOT touch pinned"); **referenced skills treated like pinned** (never auto-touched, 339); **never-used skills are NOT treated as stale** ("absence of evidence, not evidence of staleness" — only age them out after the full stale window, 360-375); **reactivation** — a stale skill used again flips back to active (378); an **LLM curator pass** consolidates/absorbs/dedups overlapping skills, archiving only "truly stale, irrelevant, or obsolete" ones (441-566).
**STEAL for a MANAGER that learns to supervise:** the ACTIVE/STALE/ARCHIVED + pinned/referenced-protection + reactivation state machine is a proven design for accumulating "playbooks" (how to triage a stuck worker, how to dispatch task-type X) **without** letting bad/duplicate skills rot the set — the exact risk of an auto-learning manager. Also steal **built-in per-session/per-model token accounting** as a first-class table (cost observability for free).

### 2F. The main turn loop — `agent/conversation_loop.py::run_conversation` ✅ (read directly, lines 1228-1558; helper map grep'd)

The core loop is genuinely production-grade — the value here is the *edge-case robustness*, which is what makes a harness top-tier vs. a toy. Read directly:

- **Per-turn prologue is extracted to `build_turn_context()`** (agent/turn_context.py): stdio guarding, retry-counter resets, user-message sanitization, todo/nudge hydration, **system-prompt restore-or-build**, **preflight compression**, the `pre_llm_call` plugin hook, **external-memory prefetch**, and crash-resilience persistence — all once-per-turn. (conversation_loop.py:1289-1329) *Clean separation of "set up the turn" from "run the loop."*
- **Dual-bounded loop:** `while (api_call_count < max_iterations AND iteration_budget.remaining > 0) or _budget_grace_call` — count *and* budget, plus a **"grace call"** that gives the model exactly one more iteration after budget exhaustion, then forces exit. (conversation_loop.py:1402, 1428-1437)
- **Live interrupt + redirect + steer:** each iteration drains `_drain_pending_redirect()` (mid-turn user corrections, injected into messages + persisted, 1403-1411) and checks `_interrupt_requested` (clean break with a diagnostic reason, 1417-1422). A **`/steer` drain** injects text that arrived *while the model was thinking* into the last tool message before the next request — preserving role alternation, with fallback re-queue if there's no tool message yet. (1473-1522) This is real-time human-in-the-loop steering.
- **Per-iteration checkpoint:** `_checkpoint_mgr.new_turn()` snapshots for crash resilience/undo. (1413-1414)
- **`step_callback` fires an `agent:step` gateway event** with the previous tool batch (names/args/results) — this is how the always-on layer observes worker progress without being in the loop. (1439-1465) *Directly relevant to our daemon watching the brain.*
- **Cache-efficient tool-arg sanitization:** `_sanitize_tool_call_arguments()` uses an **identity-keyed validation cursor** so already-validated history args aren't re-parsed every iteration; compression/undo that rewrites the list breaks the prefix match and forces a re-scan from the divergence point. (1530-1553) — a concrete KV-cache-preservation technique in the hot loop.
- **Defensive role-alternation repair** before every API call (fixes `tool→user`/`user→user` tails). (1555-1558)
- **Deep production counters:** `max_compression_attempts` (config `compression.max_attempts`, default 3) shared by the pre-API pressure gate, 413/overflow retry, and post-tool compaction gate (1347-1356); `length_continue_retries`, `truncated_tool_call_retries`, `codex_ack_continuations`; **`_auth_pool_refresh_counts`** caps same-entry OAuth refreshes so a persistent upstream 401 can't spin forever (1374-1379); **`_turn_exit_reason`** diagnostic string for every exit path (1357).
- **Verification gate:** `_pending_verification_response` holds a composed answer back for a verification pass; if the continuation exhausts budget, it's still surfaced as the best available result rather than lost. (1358-1368)
- **Pluggable runtimes:** an `api_mode == "codex_app_server"` branch hands the whole turn to a Codex subprocess; MoA (mixture-of-advisors) fan-out is supported with rebase-onto-compacted-transcript. (1388-1400, 1369-1372)

**STEAL for our harness (applies even though our brain call is short-lived — a short-lived call still runs a tool loop):** dual iteration+budget bound with a grace call; `_turn_exit_reason` on every exit; the `step_callback`/`agent:step` event so our Go daemon observes each tool step; identity-keyed tool-arg validation cursor for cache stability; defensive role-alternation repair; a bounded `max_compression_attempts`; auth-refresh spin cap. **SKIP:** MoA, Codex-app-server runtime, TTS streaming — Hermes-specific.

### 2E. Orchestration/always-on — being read now by a clavis→qwen agent (dodging the Anthropic 429)
Next cycles (grep-level): `gateway/` (daemon liveness, warm-agent-per-chat, restart/recovery), `agent/subagent_lifecycle.py` + `tools/delegate_tool.py` (delegation registry, depth/concurrency, durability across restart), `cron/` (durable scheduler), `tools/environments/{local,ssh,docker,modal}.py` (remote execution — multi-VM relevance). Earlier finding stands: supervision state (subagent registry) is **in-memory, wiped on restart** — the key gap our Go ledger fixes.

---

### (original scaffold — target files) — retained for reference

Target files in the local clone `~/main_coding/others/hermes-agent` (sparse-checked-out Python engine, 66M):
- **Main loop & context:** `agent/conversation_loop.py`, `context_engine.py`, `context_compressor.py`, `conversation_compression.py`, `manual_compression_feedback.py`, `iteration_budget.py`, `bounded_response.py`, `agent_init.py`.
- **Memory & learning:** `hermes_state_*.py` (SQLite+FTS5), `agent/memory_manager.py`, `memory_provider.py`, `curator.py`, `learning_graph.py`, `learning_mutations.py`, `insights.py`, `skills/`.
- **Orchestration & always-on:** `gateway/` (session/routing/daemon), `agent/subagent_lifecycle.py`, `tools/delegate_tool.py`, `delegation_context.py`, `cron/`, `tools/environments/` (local/ssh/docker/modal).

Known from earlier source reads (to be expanded): compaction fires at **75%** of context, protects head + ~20K-token tail, never compacts user messages; optional micro-compaction folds one oldest exchange/turn (defrag at 2000-token summary). Durable = SQLite+FTS5 transcripts + curated MEMORY.md/USER.md (whole-file into system prompt) + search-based prefetch capped ~6K chars. **Supervision state is volatile** — subagent registry is an in-memory dict wiped on restart (1hr result retention), external workers tracked only in lossy context. Cron jobs are durable (`~/.hermes/cron/jobs.json`).

_To fill: exact loop step-through, curator/skill-drift controls, gateway restart/recovery, delegation depth/concurrency, execution-backend remote drive._

---

## Part 3 — The blueprint for OUR harness ✅ v1 (synthesized from Part 1 + Hermes source; market-repo refinements pending qwen-quota reset)

Full Hermes deep-dives on disk: `hermes-analysis/conversation_loop-analysis.md` (690 lines) and `hermes-analysis/context_compressor-analysis.md` (240 lines) — both complete, with STEAL/SKIP tailored to a short-lived ledger-rebuilt manager. Market-repo teardowns (autogen manager-pattern, claude-squad fleet, graphiti memory, openai-agents handoffs, vibe-kanban) FAILED on the qwen 5-hour quota and are queued for a staggered re-run; sections tagged ⏳ will be refined from them.

### 3.1 The shape: durable Go daemon + short-lived LLM brain

**Name: `arco`** — the violin instruction to play *with the bow*: how a player draws sound and control out of the strings. Fits the trio `clavis` (Latin "key", the launcher) · `herdr` (the herder) · `arco` (the bow that commands the ensemble): arco directs the worker fleet the way a bow commands the strings. Ships as a Go binary (its own, or a `clavis arco` subcommand).
```
arco (Go daemon, systemd Restart=always)  ── owns ALL durable truth
  ├─ event intake:  herdr plugin-hook execs → posts agent-state-change JSON to a local socket/HTTP
  ├─ ledger:        SQLite = worker registry (id, vm, workspace, task, state, last_event, summary, playbook_refs)
  ├─ messaging:     Telegram bot (phone in/out)
  ├─ cross-VM:      each VM's herdr hook posts to ONE central daemon endpoint (herdr --remote won't carry events)
  └─ brain trigger: on a decision-worthy event, assemble a SMALL context and invoke a SHORT-LIVED LLM call
The LLM never runs for days → no Node/heap leak, no always-on token burn, a crash is a retried subprocess.
```

### 3.2 Managing the "main agent" (each short-lived brain call) — the design
The brain call is stateless across events; its coherence comes from what we *assemble*, not from a living context. Construction recipe (each field cited to the mechanism it steals):
1. **Always-hot core block** (MemGPT core-memory): identity + mandate + standing constraints + the **live worker roster pulled fresh from the ledger** every call. This is the manager's "who am I / what am I running" — never paged out, always rebuilt from durable truth.
2. **Recitation** (Manus todo.md): the current worker/TODO status table sits at the *tail* of the prompt so goals are in recent attention, not lost-in-the-middle.
3. **Reference-based, not payload-based** context (Manus files / Hermes archival): pass worker IDs, file paths, PR URLs, log locations — never inline payloads. The brain calls a tool to fetch a detail only if it needs it (just-in-time retrieval).
4. **Deterministic, LLM-free context prune BEFORE the model call** (Hermes `prune_tool_results_only`, `context_compressor.py:3018-3085`, `_summarize_tool_result` :1128-1288): dedup identical tool outputs, collapse big results to a 1-liner, truncate huge args keeping JSON valid. Even a short-lived call's assembled context bloats from tool output — a cheap idempotent pass is pure win. **This is the single most transferable compaction idea.**
5. **KV-cache-stable prefix** (Manus + Hermes select_context invariant): stable system prefix, no per-second timestamps, append-only; the identity-keyed tool-arg validation cursor (conversation_loop.py:1530-1553) to avoid re-parsing.
6. **Keep failures in context** (Manus): retain a worker's last error/stack so the manager stops re-dispatching a broken instruction.
7. **Bounded tool loop** for the call: dual iteration+budget bound with a grace call, `_turn_exit_reason` on every exit, role-alternation repair, auth-refresh spin cap (conversation_loop.py STEAL §).

**Explicitly SKIP (per the compressor analysis — these only pay off in a long-lived session):** micro-compaction / rolling-transcript rewrite, session rotation / lease / commit-fence machinery, preflight-deferral cross-turn calibration. Keep only the *rolling-summary idea as a ledger field*, and the *archival idea* (pointers), not the plumbing.

### 3.3 The harness (daemon) — what makes it top-tier
- **Ledger as source of truth** (fixes Hermes's #1 gap: its supervision registry is in-memory, wiped on restart). Worker state survives restart, powers cross-VM aggregation, and is what every brain call reads.
- **Per-event checkpoint + durable write-ahead** (LangGraph checkpointing; Hermes `_checkpoint_mgr`): every state transition is committed before acting, so a daemon crash resumes exactly.
- **`agent:step`-style observation** (Hermes step_callback, conversation_loop.py:1439-1465): the daemon observes each worker's tool steps via herdr events *without being in any loop* — this is the whole point of the event-driven design.
- **Built-in per-worker/per-model token+cost accounting** as a first-class ledger table (Hermes `session_model_usage`) — cost observability for free at swarm scale.
- **Durable cron** (Hermes `~/.hermes/cron/jobs.json`) for scheduled manager tasks that fire while you're offline, delivered to Telegram.
- **Swappable brain**: claude headless (via clavis) for hard decisions, qwen/cheap model for routine routing — model-agnostic like Hermes. (Cost is a non-differentiator; worker tokens dwarf it.)

### 3.4 Learning over time (the compounding edge) ✅
**Playbook store** modeled on Hermes's curator state machine (`curator.py`): manager-authored "how to triage a stuck worker / dispatch task-type X" docs move ACTIVE→STALE→ARCHIVED by inactivity, with **pinned + referenced protection**, **reactivation on reuse**, and an LLM consolidation pass — the proven anti-drift design. Gate promotion behind human approval. This is what makes the manager *get better* rather than just run.

### 3.5 The manager/orchestration pattern ✅ (autogen + openai-agents + claude-squad + vibe-kanban all converge)
The four market teardowns independently point at ONE design. Full cites in `hermes-analysis/{autogen,openai-agents,claude-squad,vibe-kanban}-analysis.md`.

**The core contract (openai-agents + autogen): a worker response is NEVER applied directly.** It's classified into a typed step — `StepResult{RunAgain | Handoff | FinalOutput | Interruption(approval)}` — then reconciled through a **manager-owned state machine**, persisted, guarded, and only then allowed to drive the next dispatch. The daemon owns routing; workers don't route themselves.

**When the LLM brain is invoked (autogen `SelectorGroupChat` ⊕ `MagenticOneOrchestrator`):**
- **Worker selection** — brain names the next worker; parse + validate the name; **retry with feedback on invalid selection** (never trust the raw pick).
- **Stall detection → replan** — the ledger tracks `is_progress_being_made` / `is_in_loop` / stall-count; after N stalls the brain re-plans. (This is the manager's anti-loop safety.)
- **Ambiguous state classification** — when herdr events + transcript + liveness don't clearly say done/blocked, a short-lived brain call decides.
- Otherwise the daemon acts deterministically from the ledger — **no LLM call**.

**Termination** = a stateful, composable `TerminationCondition` (AND/OR), checked **delta-only** (new events since last check), not by re-reading the whole log (autogen `_termination.py`, `_base_group_chat_manager.py:195`).

**Handoff vs subtask (openai-agents):** *handoff* = ownership of the job transfers to a new worker (daemon appends a transfer event, changes `current_owner`); *subtask* = spawn a bounded helper that returns output to the caller. Model both explicitly in the ledger.

**Execution substrate (claude-squad + vibe-kanban):** worktree-per-worker with recorded base commit + before/after HEAD OIDs for diffing; one process/tmux (or herdr pane) per worker with **human-attach escape hatch**; cheap ~500ms metadata poll for freshness (full diff for the focused worker, numstat for the rest); **per-agent log normalization** into one `NormalizedEntry` event stream; **bidirectional exit signalling** (worker can announce done; manager can request graceful shutdown); **orphan cleanup on boot** (mark `running`→`failed`). Workers are **command templates**, not hard-coded claude (profile abstraction).

**Events over polling (autogen pub/sub + vibe-kanban MsgStore):** workers/herdr publish state-changes to a topic; the manager subscribes. Polling is the fallback freshness loop, not the primary signal.

Draft stance holds and is reinforced: **single continuous manager state-machine** (Cognition: don't multi-agent the manager itself); workers are the parallel fan-out, isolated, returning 1–2K distilled summaries.

### 3.7 The ledger schema + brain contract (concrete synthesis) ✅
**SQLite worker ledger** (WAL, single writer — vibe-kanban's warning) = autogen's `message_thread` + `model_context` + task-ledger, made durable. Tables:
- `workers(id, vm, workspace, worktree, base_commit, program_template, process/tmux_id, state, stall_count, owner_job, last_event_at, summary)` — **generated IDs, not titles** (claude-squad SKIP); **rich states**: `starting, running, waiting_for_approval, waiting_for_user, blocked, completed_candidate, completed_verified, failed, paused, killed` (claude-squad's `Ready` catch-all is too coarse for automation).
- `events(id, worker_id, turn_id, kind, payload, valid_from, valid_until, recorded_at)` — **per-turn append-only event log** (openai-agents), with **bi-temporal columns** (graphiti: query "what was true at T", handle corrections) — NOT a mutable transcript string.
- `jobs(id, owner_worker, plan, facts, progress, stall_count)` — the task ledger the brain reads/updates.
- `approvals(id, worker_id, action, status, requested_at, decided_at)` — **approval-as-durable-state**: record it, notify Telegram, resume on decision; never block inside a tool (openai-agents).
- `usage(worker_id, model, tokens, cost)` — first-class cost accounting (Hermes `session_model_usage`).
- `playbooks(...)` — §3.4 curator store.

**State detection = fusion, not one signal** (claude-squad SKIP): combine herdr-hook events + Claude transcript parse + process liveness + file/HEAD changes + short-lived LLM classification — no single source is truth.

**Crash recovery:** per-turn persistence + serializable brain state (current plan/facts/stall-count) + orphan cleanup on boot = resume exactly (autogen serializable state, openai-agents per-turn persist, LangGraph checkpoint).

**Memory:** stay on Hermes's FTS5 + core-memory-blocks; **steal graphiti's *temporal* idea (bi-temporal columns) but SKIP the graph DB** — Neo4j/BFS/per-fact-embeddings/community-detection are disproportionate for tracking 5–50 workers; indexed JOINs + BM25 + optional embedding suffice.

### 3.8 Phasing (unchanged, now better-grounded)
1. **Phase 1 — concierge (near-zero build):** claude on a Max subscription, single VM, over the herdr-hook → Telegram loop. Validates the push path. *Never meter on the Opus API.*
2. **Phase 2 — the clavis-Go daemon:** ledger + event intake + Telegram + short-lived brain, built with §3.2–3.3.
3. **Phase 3 — swamp:** cross-VM aggregation; provision multiple orgs for worker rate limits; add the playbook store.

**Open items for Long:** confirm the Go-daemon direction; Phase-1-first vs straight to Phase-2; whether to evaluate turnkey **orca** (would demote herdr) before building; and whether the learning/playbook loop is in-scope for v1 or deferred.

---

### 3.9 Additional patterns — persistent-agents (OpenClaw/Khoj) + coding-agent survey (opencode/cline/OpenHands/agent-sdk) ✅
Full cites in `hermes-analysis/persistent-agents-analysis.md` and `other-implementations.md`. High-value additions beyond §3.5/3.7:
- **Dual-loop shape (OpenClaw):** an **inner** tool loop (drive the current task) wrapped by an **outer** follow-up loop (wait for new work) — this *is* the always-on manager's shape. Pair with **separate steer vs follow-up queues** (steer promoted at safe turn boundaries; queued input waits until idle) to prevent priority inversion.
- **Structured compaction/handoff summary schema (OpenClaw + survey #5):** a fixed template — **Goal → Constraints → Progress → Decisions → Next Steps → Critical Context (exact paths/symbols/commands/errors)**. Update the previous summary in place rather than regenerating; track file-ops across the boundary. Use this exact schema for our brain-call context assembly and for worker handoffs.
- **Durable prompt admission (survey #1):** admit work to a durable **inbox with stable message IDs** *before* waking the runner; make retries idempotent. (Pairs with the ledger.)
- **Record tool call BEFORE side effect (survey #12):** persist "intended call" → execute → persist typed success/failure, so replay distinguishes *model asked* from *tool ran*. Essential for a crash-safe supervisor.
- **Deterministic overflow fallback (survey #7):** agentic summarization for normal compaction, but provider-overflow recovery must use a **deterministic projection that cannot itself overflow**.
- **Loop hook points (survey #11 + OpenClaw lifecycle hooks):** `prepareTurn / beforeModel / beforeTool / afterTool / status` — so supervision/policy/approval code never forks the main loop. Also **`prepareNextTurn` dynamic model selection** = cost-aware brain routing (cheap model vs claude per event).
- **Agent-managed cron + reminder-context injection (OpenClaw + Khoj):** the manager creates/edits/deletes its **own** scheduled follow-ups; when a scheduled task fires, **inject the recent conversation context** so it resumes coherently; accept natural-language → cron ("remind me every Monday"); **tool-allowlist scheduled tasks**; give each automation its own session. Khoj adds **SQLite-backed job persistence (APScheduler pattern)** + graceful-shutdown hook + manual-trigger endpoint for testing.
- **Replayable event stream (survey #14):** seed history from durable events, reconnect with a **"since" cursor**, dedup by event ID before side effects — powers both UI and crash-restart.
- **Explicit delegation gates (survey #16):** spawn/team/delegation are **privileged capabilities, off by default** in automation/yolo modes unless manager policy enables them.

---

## Appendix — research run status (2026-08-04)
- ✅ **Complete:** external teardown (Part 1); Hermes context_engine, iteration_budget, main-loop, compaction, memory/curator (Part 2A–2F); two full deep-dives on disk (conversation_loop, context_compressor).
- ✅ **ALL market teardowns complete** (re-run across DeepSeek + Codex pools, staggered 2–3 at a time after the qwen quota exhausted — per [[agent-fanout-quota-discipline]]): autogen, claude-squad, graphiti, openai-agents, vibe-kanban, other-frameworks survey (opencode/cline/OpenHands/agent-sdk), persistent-agents (OpenClaw + Khoj). 9 analysis files in `hermes-analysis/`, all folded into Part 3.
- **Provider routing that worked:** clavis qwen-1 (until its 5h quota hit) → then clavis deepseek-1 (DeepSeek) + tokamak launch codex (Codex pool). tokamak launch claude was NOT usable (routes to the same rate-limited Anthropic account).
- **Repos on disk** (`~/main_coding/others/`): hermes-agent, autogen, claude-squad, graphiti, openai-agents-python, vibe-kanban, openclaw (+ herdr). Plus `/tmp/agent-research/`: opencode, cline, goose, openhands, claude-agent-sdk-python.
