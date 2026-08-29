# ADR 0003 — Pluggable feature registry + durable context store

Status: **Proposed** (design study complete; awaiting operator approval before implementation)
Date: 2026-08-27
Supersedes/extends: [0002 — plugin format = MCP + SKILL.md](0002-plugin-format-mcp-skill.md)

## Context

Every operator-facing capability (`/scan`, `/adopt`, `/peek`, `/dispatch`, image
relay) is hand-threaded through ~5 hardcoded layers:

    engine method → telegram.Actions iface → daemon engineActions adapter
    → telegram command switch + setMyCommands + helpText → tests

Adding one capability edits all five; there is no single place a "feature" lives.
Separately, the chat brain is **stateless** (a 5-turn in-memory buffer, lost on
restart) and cannot look inside or act on the fleet without a hardcoded command.

The operator wants arco redesigned as a **core agent daemon + a harness of
swappable modules**, with features that **plug into** it, and a first-class
**context-management** subsystem: durable per-thread history the agent can
**query on demand**, windowed by time (a few days) or count (a few hundred) so
the brain prompt never overflows.

A 3-reviewer design study (three independent code-grounded opus agents; a
clavis-qwen consult stalled as before) **converged** on the design below.

## Orienting finding (de-risks the whole context piece)

Migration `0001_init.sql` (lines 107–121) **already ships**
`brain_transcript_rows(session_id, role, content, active, compacted,
source_events, tainted, created_at)` **and** a `transcript_fts` FTS5 mirror
(porter tokenizer) — and **nothing in the Go code reads or writes them** (grep-
confirmed). The durable, full-text-searchable, per-session context store the
operator wants is *reserved schema*. The context subsystem is therefore mostly
**wiring**, not new design — the single biggest risk reducer available.

## Decision

### D1 — A `Feature` is a struct of optional declarations, assembled by a `Registry`

Not a fat interface and not a plugin ABI — a compile-time Go value that fills
only the surfaces it offers:

```go
package feature

type Feature struct {
    Name    string
    Command *Command   // → telegram switch + setMyCommands + /help + CLI subcommand
    Tool    *Tool      // → brain tool-loop + MCP export
    Sweep   SweepHook  // → an extra reconcile.Sweep leg (usually nil — see non-goals)
    Skill   *Skill     // → SKILL.md injected into workers (ADR 0002)
}

type Command struct {
    Name, Help, Usage string
    Run func(ctx context.Context, in CmdInput) (reply string, err error)
}
type CmdInput struct{ Arg string; ThreadID int64; SessionID, Actor string }

type Tool struct {
    Name, Desc string
    Schema     json.RawMessage
    Access     Access // Operator | BrainSafe (read-only/idempotent) | BrainAct (mutating)
    Call       func(ctx context.Context, args json.RawMessage) (string, error)
}
```

`Command.Run` returns `(string, error)` — the shape ~90% of today's `commands.go`
already has (`cmdScan`/`cmdPeek`/`cmdKill` return a string `b.reply` prints).

Features are built at daemon assembly from a `Deps` bundle — the DI seam the
brief asks for; each harness service stays behind its existing small port
(`core.Store`, `core.VMClient`, `notify.Sender`, `brain.Runner`, `ContextStore`,
`*memory.Store`, and a **narrow** `EnginePort` that succeeds today's fat
`telegram.Actions`). A `Registry` binds one declaration to every surface:
telegram `handleCommand` consults `reg.Commands()`; `setMyCommands`/`helpText`
are **generated**; the brain tool-loop reads `reg.Tools()`; the MCP server
iterates `reg.Tools()` (ADR 0002 satisfied — MCP is one consumer, not new code).

### D2 — Brain tool-loop (`Converse`): text-protocol, single-tool-per-round, hard-capped

`clavis` is a plain-text CLI with **no native tool-use API**, so the only fit is
a text-protocol loop. Keep `brain.Invoke` one-shot; wrap it:

```go
func (e *Engine) Converse(ctx context.Context, sess, prompt string, tools []feature.Tool) (string, error)
```

Each round renders the tool catalog + durable context (D3) + the message and
asks the model to emit **either** `{"final":"…"}` **or**
`{"tool":"scan","args":{…}}` (extracted by the existing balanced-brace
`ParseStep` parser). On a tool object arco executes via the registry and
re-prompts with the result appended; cap **3 rounds**, each a metered
`brain.Invoke` under the existing `BrainRate` cap. This makes chat agentic —
"peek into it" = the brain calls `scan` then `peek` with **zero hardcoded
intent**, retiring `chatPrompt`/`herdrSessionFacts`/`chatHist`.

**Security (non-negotiable, unchanged boundary):** `Access` is the gate.
`BrainSafe` tools (scan/peek/status/fetch_context) are always callable;
`BrainAct` tools (dispatch/kill) are callable **only** when
`sessionMode(sess).Allows(ActBrainAct)` and otherwise **degrade to an
escalation** exactly as `applyStep` does today (`brain_apply.go:310`).
`Operator` tools (pause/grant/answer/confirm) are **never** in `reg.ForBrain()`
or `ForMCP()`; escalation decisions stay human-only (the `AnswerQuestionBrain`
no-grant + `Tainted` invariants hold). The loop honors `Paused()`.

### D3 — Context store on the existing `brain_transcript_rows` + `transcript_fts`

A durable, append-only, per-session, full-text-searchable conversation log —
**not** folded into the `events` table (that drives the fusion reducer, crash-
recovery scans, and rate admission, and must stay a pure audit log). New ports:

```go
// core.Tx (append-only; Content is Scrub()'d at this chokepoint, like events):
AppendMessage(m Message) (id int64, err error) // also inserts into transcript_fts
// core.Reader:
RecentMessages(sessionID string, since time.Time, limit int) ([]Message, error)
SearchMessages(sessionID, query string, limit int) ([]Message, error) // FTS5 MATCH
```

- **Thread key = `session_id`** (a Telegram topic is 1:1 with a session via
  `sessions.tg_topic_id`). General-topic chat (no session) binds to a fixed
  **console sentinel session** (mirrors the pool sentinel) → **zero migration**.
  (Fallback: an additive `0009` `thread_id` column — additive only, no CHECK
  rewrite — if the sentinel proves awkward.)
- **Windowing** realizes "a few days OR a few hundred, whichever bounds it":
  `RecentMessages(sid, now-72h, 300)`; the `LIMIT` is the hard ceiling. Retrieved
  rows feed the **existing** `assembleContext` 16 KB `contextBudget` as a third
  section (drop-oldest-that-doesn't-fit) — so the prompt can never overflow.
- **On-demand retrieval** = a built-in `fetch_context`/`history` **BrainSafe
  tool** wrapping `SearchMessages`/older pages — the brain fetches more itself
  instead of everything being force-fed.
- **Rollup reuses `Session.ContextSummary` + `ContextRev`** (already present):
  when live rows cross the cap, one `brain.Invoke` digests them into
  `ContextSummary` (already rendered by `assembleContext`) and flips folded rows
  `active=0, compacted=1`. A retention reaper (a sweep leg, like `ReapLeases`)
  hard-deletes `compacted=1` rows past a horizon; live rows are never deleted.
- Replaces the in-memory `chatHist`/`recordChatTurn`/`maxChatTurns` — history now
  survives restart and is fleet-wide queryable.

### D4 — Two memory tiers behind one harness, both scrubbed

`memory.Store` (USER.md/MEMORY.md) stays the **always-hot, human-authored,
tiny** tier (human-only-write is a security boundary). `ContextStore` is the
**large, machine-generated, queried-on-demand, prunable** tier (`tainted` marks
brain-sourced rows). `assembleContext` layers both. Not merged — merging would
either make curated memory prunable or make conversation always-hot/unbounded.

## Non-goals (explicit — the codebase values small surface + no bloat)

- **No plugin ABI / dynamic loading** (`.so`/go-plugin). Features are compile-
  time values registered at assembly — same trust model as today.
- **No RAG / vector store.** FTS5 + a time window is right at the <10³-message
  single-operator scale.
- **No CHECK-constraint rewrites** on frozen tables; additive migrations only.
- **`SweepHook` ships last, or not at all.** The core sweep legs (`checkStall`,
  `finalize`, `reapOrphanedAgents`) are welded to the CAS/tx machinery and stay
  hardcoded; only a genuinely additive read-mostly leg (a daily-digest card)
  would justify a hook. Resist that abstraction until there's a real second user.
- Ledger stays **append-only**; the reviewed reconcile core (fusion, CAS, D9,
  leases, escalation grant boundary) is **never touched** — features wrap its
  existing methods.

## Phased migration (no big-bang; each phase independently shippable + green)

- **Phase 0 — seams, no behavior change.** Land `ContextStore` ports,
  `feature.Registry`, `Deps`, `EnginePort`; add a fall-through in
  `handleCommand`'s `default:` to consult `reg.Commands()`. Empty registry ⇒
  identical behavior. This is the coexistence seam (switch + registry both live;
  each command in exactly one).
- **Phase 1 — context store (the operator's headline).** Wire the transcript
  tables (AppendMessage/RecentMessages/SearchMessages, scrubbed + FTS); swap the
  in-memory `chatHist` for the durable store; feed it into `assembleContext`.
  Foundational, near-zero risk (no reducer/security path touched).
- **Phase 2 — registry proof.** Port **`/scan` first** (read-only, lowest blast,
  already surfaced in chat) as a `Command`+`Tool`. `setMyCommands`/`/help`
  auto-generate; MCP would export it. One module, five surfaces.
- **Phase 3 — agentic chat.** Swap `handleChat`'s `BrainReply` for `Converse`
  once `scan` + `fetch_context` are tools. "peek into it" now works with zero
  hardcoded intent. Retire `chatPrompt`/`herdrSessionFacts`.
- **Phase 4 — port the rest** one at a time: `/peek`, `/adopt`, `/dispatch`,
  `/kill`, `/diff`, image relay. Each deletes a `case` + an `Actions` method +
  a `menuCommands` line together. Mutating features keep their estop/capability
  checks inside the closure.
- **Phase 5 — delete the scaffolding.** Remove the big switch, shrink
  `telegram.Actions` to `EnginePort`, generate `helpText` from the registry, and
  stand up the MCP server as a pure registry consumer (ADR 0002).

## Risks

- Redaction MUST run at `AppendMessage` (write-time) and pre-prompt, or chat
  becomes a secret-exfil path into brain prompts.
- The 16 KB `contextBudget` stays the single overflow guard; `Query`/`Recent`
  limits are hard-capped server-side so no caller can request 10⁵ rows.
- The brain must never gain autonomy via tools: mutating tools fail-closed to
  escalations; `Paused()` + `BrainRate` bound the loop; each round is metered.
- Migrations additive only; `Migrate` verifies checksum drift on frozen files.

## Consensus & dissent from the study

All three reviewers independently found the transcript scaffold and converged on
D1–D4. Minor divergences, resolved here: **feature shape** — struct-of-optional-
declarations (chosen; least bloat) over multiple small interfaces; **thread key**
— session_id + console sentinel (chosen; zero migration) over an additive column;
**first feature** — context-store/chat-history first (chosen as Phase 1; it is
foundational) then `/scan` as the first registry feature; **SweepHook** — defer
(chosen) over ship-minimal.
