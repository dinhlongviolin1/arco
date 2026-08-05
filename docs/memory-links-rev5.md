# Cross-project memory links — design decision (rev 5)

> Resolves the "node/graph memory" question. Reviewed independently by **opus, fable, and qwen3.8-max**
> (each read the plan + a shared brief). **Unanimous verdict: SIMPLIFY** — three model families, same call,
> near-identical designs. This doc is the consolidated decision; the increment folds into **Task 24 / 24a**.

## The question
Should arco's memory be a general typed **node/graph** (repos, projects, external OSS projects as nodes;
typed many-to-many edges; cross-project linking), backed by "tables OR md — a good combination"?

## Decision: **links, not a graph**
The cross-project **linking need is real and genuinely uncovered** by the current 24a/b/c — nothing links
"external OSS project X's design" → "my project Y that adapts it", nor records *origin*-provenance
("adopted bi-temporal edges from Graphiti"). But a general typed graph is **over-engineered** for a solo,
single-writer, short-lived-brain daemon, and it fights invariants the rev-4/rev-5 freeze already locked.
Ship the value as **`[[wikilinks]]` in the existing topic files + a *derived* backlink index**, rebuilt at
the `mem_rev` bump exactly like FTS5. md stays the source of truth; SQLite holds only a derived index.

### Why not a general graph (all three reviewers converged)
- **Edges have no author.** You won't hand-maintain a normalized edge table; the **brain is banned** from
  writing memory (rev-4.1 decision #3 / rev-5 **B13** — `author=brain` auto-apply is an injection→persistent
  -memory hole); code can only *derive* mechanical edges. Every automated-graph system (Zep/Graphiti, mem0-graph)
  pays for edges with an **always-on LLM extraction pipeline** — the exact warm process arco bans.
- **The ≤8KiB hot budget forbids multi-hop *by construction*.** A 2-hop neighborhood fits in no tier
  (USER.md ≤4KiB + MEMORY.md ≤4KiB; JIT ≤6K chars; FTS recall top-5 ≤500 chars), so the thing that makes a
  graph a graph is dead weight here.
- **"tables OR md" is a dual-truth trap** — two mutable sources of truth is a permanent sync problem. Pick
  **md as truth, tables as a rebuilt-from-files derived index** (mirrors the rev-4 "`sessions.permissions`
  = derived cache only" pattern).
- **Cross-project node = exfil + least-privilege blast radius.** One hop bridges a private node into a
  public-repo worker's brain prompt (rev-5 **B4** names the brain prompt the largest exfil surface).
- **It would be the 3rd/4th graph** (session `parent_session` tree, capability tree, `supersedes` chains
  already exist) — retrieval-ambiguity + maintenance load for one person.
- **A typed `rel` CHECK-enum would hit the PASS-0 freeze** — wrong relation types later cost a table rebuild.

### Prior art (what actually works at this scale)
MemGPT/Letta = tiers + tool-driven paging, no graph (arco's T1/T2/JIT already copies this). mem0 = flat
fact store is the **production default**; the graph variant is the opt-in minority. GraphRAG = wins only
global "sensemaking," loses on pointed queries, expensive to maintain. Zep/Graphiti = genuinely strong, but
**requires the banned always-on extraction pipeline**. Obsidian/Roam = the one *human-scale* graph that
sticks — **untyped `[[links]]` + a mechanical backlink index**; typed-ontology plugins get abandoned. Claude
Code's own auto-memory (`MEMORY.md` + `[[wikilinks]]`, no graph DB) is an existence proof at this exact scale.
Consistent finding: a *maintained* typed graph beats "chunks + metadata + links" only when edge extraction
is automated **and** queries are multi-hop/global — arco has neither in P2.

## The design (24a increment)

**Node = a topic file; edge = a `[[wikilink]]` in the file body; the edge index = derived, rebuilt at `mem_rev`.**

- Topic files gain frontmatter `scope:` (`global` | project slug | `oss/<name>`) and optional `kind:`
  (`project` | `concept` | …). A "node for an OSS project" is literally `oss/graphiti.md` with
  `scope: oss/graphiti`. **No `nodes` table** — the `MEMORY.md` index line *is* the node record at <10² nodes
  (add a table only if a query later proves it needs one).
- Links are authored **in the moment of writing** (Obsidian's proven, near-zero-marginal-cost model), via
  `ApplyMemoryDiff(diff, decidedBy)` — the frozen, human-`decided_by`, `author=brain`-rejecting write
  interface (rev-5). Links are a **byproduct of the one sanctioned, human-approved write**; the brain never
  writes edges.

```sql
-- DERIVED index, rebuilt from the md files at each mem_rev bump (the one sanctioned prefix-cache break).
-- No write API; rebuilt-from-files only (assert with a test, like the migrate-from-fixture test).
CREATE TABLE memory_links (
  src_topic  TEXT NOT NULL,               -- topic file slug == node id
  dst_topic  TEXT NOT NULL,
  rel        TEXT NOT NULL DEFAULT 'ref',  -- MVP: 'ref' only; name the relation in prose next to the link
  mem_rev    INTEGER NOT NULL,             -- pins byte-stability; matches existing mem_rev
  PRIMARY KEY (src_topic, dst_topic, rel)
);
CREATE INDEX idx_memory_links_dst ON memory_links(dst_topic);  -- the backlink index
```

**Retrieval — depth-1, no warm server, byte-stable:**
- `memory_read(topic)` returns the file **+ its direct out-links and backlinks**, deterministically sorted
  (`(rel, dst_topic)`), capped, inside the existing 6K-char budget → byte-stable per `mem_rev`.
- **Multi-hop = the brain choosing to issue another `memory_read`** (agent-driven paging, the MemGPT pattern
  the plan already uses) — never an engine traversal. Each hop is an explicit, logged, budgeted tool call.
- `MemorySearch` (FTS5) unchanged; a backlink line appears in hit snippets; optionally re-rank hits that are
  a 1-hop neighbor of the session's `repo` node (pure deterministic re-rank).
- **`AssembleContext` never traverses.** A "See also" line may be rendered at write time into the T2 index,
  pinned to `mem_rev` — byte-stable for free.

**Safety (inherits the rev-5 invariants — non-negotiable):**
- `Scrub` at write-time on every topic-file writer **and before `Invoke`** (B4).
- OSS-derived facts carry `trust: external` and are **`Tainted` (advisory-only)** — same injection closure as
  brain answers and rollup summaries (B12/M19); an adversarial OSS README must never become an authority-bearing
  or auto-applied fact.
- Cross-scope recall is gated behind a **default-off `memory.cross-project` capability** in
  `capability_catalog` (same pattern as fleet ops); default recall scope = `{global, session.repo}`.
- P2 autonomy is shadow/draft, so a human sees every memory-derived answer — safe by construction in P2.

## MVP cut (ships inside 24a; days, not weeks)
1. `scope:`/`kind:` frontmatter + a scope tag on `MEMORY.md` index lines (convention + parser).
2. `[[wikilink]]` convention in topic files; `rel='ref'` only (types expressed in prose).
3. `memory_links` derived table rebuilt at `mem_rev`; `memory_read` returns 1-hop neighbors + backlinks;
   backlink line in search hits.
4. Seed with the real case: `oss/<project>.md` ↔ `arco` topic links.

**Defer:** scope-gated recall + the `memory.cross-project` capability → PASS-3 (with the rest of security
hardening); hindsight *proposing* links → 24b pending queue (zero auto-apply, same rails as memory diffs);
dangling-link pruning + link aging → 24c curator. **Drop entirely:** typed edge ontology beyond `ref`, a
generic `nodes` table, a graph traversal/query engine, multi-hop retrieval, graph visualization, embedding-based
edge inference.

## Kill-criterion (instrument from day one)
Log neighbor-hop `memory_read` calls and whether link-discovered source IDs appear in brain answers /
`StepResult`s. Rip it out if, over ~4–8 weeks of real P2 use: the 2nd-hop follow rate stays **<~10%**, OR you
author no new links after seeding / the table holds **<~20 rows**, OR **>~20%** of links dangle at a rebuild.
Rollback = delete the derived table; the `[[wikilinks]]` remain as inert prose in the files → **zero data
migration.** That reversibility is the decisive reason to build *this* and not the graph.

## Open questions for the maintainer
1. **`nodes` table: skip it (recommended) or add it now?** Recommend skip — MEMORY.md index line = node record.
2. **`rel`: untyped `ref` MVP (recommended) or typed enum now?** Recommend untyped — dodges the CHECK-freeze;
   types can be added later without a rebuild.
3. **Does the `scope` tag go in the always-hot index?** Only if it doesn't measurably drop `cache_read`
   (watch `usage`); otherwise keep `scope` in frontmatter only.
