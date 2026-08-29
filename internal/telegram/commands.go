package telegram

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/prompts"
)

const helpText = `arco — drive the fleet from Telegram

read:
  /status                fleet summary (estop, active workers, pending)
  /vms                   the attached VMs (fleet hosts)
  /scan                  live herdr agent sessions across the fleet
  /sessions              list active sessions + their topics
  /workers               list workers by state
  /diff <worker>         a worker's redacted diff (id prefix ok)

act:
  /dispatch <repo> <task>   spawn a worker on <repo> to do <task>
                            (in an issue's topic → adds an aspect to that issue;
                             in General → starts a new issue; --vm <name> to pick a VM)
  /adopt [ref]              track an existing herdr session (no ref → all untracked)
  /kill <worker>            terminate a worker (id prefix ok)
  /pause  /resume           emergency stop on / off

answer:
  tap the buttons on a decision card, or just TYPE your answer inside
  that worker's topic. Send a photo in a topic to hand it to the worker.

chat:
  type anything else and arco's brain replies.`

// handleCommand runs a slash-command and replies in the same topic.
func (b *Bot) handleCommand(ctx context.Context, m *Message, text string) {
	fields := strings.Fields(text)
	cmd := strings.ToLower(strings.SplitN(fields[0], "@", 2)[0]) // strip /cmd@botname
	arg := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))

	switch cmd {
	case "/help", "/start":
		b.reply(ctx, m, helpText)
	case "/status":
		b.reply(ctx, m, b.fleetStatus())
	case "/vms":
		b.reply(ctx, m, b.renderVMs())
	case "/scan":
		b.reply(ctx, m, b.cmdScan(ctx))
	case "/adopt":
		b.reply(ctx, m, b.cmdAdopt(ctx, arg))
	case "/sessions":
		b.reply(ctx, m, b.renderSessions())
	case "/workers":
		b.reply(ctx, m, b.renderWorkers())
	case "/pause":
		if err := b.actions.Pause(ctx); err != nil {
			b.reply(ctx, m, "pause failed: "+err.Error())
		} else {
			b.reply(ctx, m, "⛔ estop engaged — no new workers, no autonomous actions (in-flight work keeps running)")
		}
	case "/resume":
		if err := b.actions.Resume(ctx); err != nil {
			b.reply(ctx, m, "resume failed: "+err.Error())
		} else {
			b.reply(ctx, m, "▶️ estop released")
		}
	case "/dispatch", "/new":
		b.reply(ctx, m, b.cmdDispatch(ctx, m, arg))
	case "/kill":
		b.reply(ctx, m, b.cmdKill(ctx, arg))
	case "/diff":
		b.cmdDiff(ctx, m, arg)
	default:
		b.reply(ctx, m, "unknown command "+cmd+" — /help for the list")
	}
}

// handleChat handles free text. Priority: (1) a swipe-reply to a specific
// escalation card answers THAT agent; (2) a bare message in an issue topic with
// exactly one pending question answers it — with several pending it refuses to
// guess and asks the operator to reply to a specific card; (3) otherwise it's a
// conversational brain reply.
func (b *Bot) handleChat(ctx context.Context, m *Message, text string) {
	// (1) swipe-reply → the exact card's escalation.
	if escID, ok := b.escForReplyTo(m); ok {
		b.answerEsc(ctx, m, escID, text)
		return
	}
	// (2) bare text in an issue topic.
	if m.MessageThreadID != 0 {
		if s, ok := b.sessionByTopic(m.MessageThreadID); ok {
			pend := b.pendingQuestions(s.ID)
			switch {
			case len(pend) == 1:
				b.answerEsc(ctx, m, pend[0].ID, text)
				return
			case len(pend) > 1:
				var sb strings.Builder
				sb.WriteString("several agents are waiting — swipe-reply to the specific card, or tap its ✅. pending:\n")
				for _, e := range pend {
					fmt.Fprintf(&sb, "• %s — %s\n", short(e.WorkerID), truncate(e.Action, 50))
				}
				b.reply(ctx, m, strings.TrimRight(sb.String(), "\n"))
				return
			}
		}
	}
	// (3) conversational brain reply.
	reply, err := b.actions.BrainReply(ctx, b.chatPrompt(ctx, text))
	if err != nil {
		b.reply(ctx, m, "🤖 chat unavailable ("+err.Error()+") — try /help for commands")
		return
	}
	b.reply(ctx, m, reply)
}

func (b *Bot) answerEsc(ctx context.Context, m *Message, escID, text string) {
	if err := b.actions.AnswerQuestion(ctx, escID, text, core.ScopeOnce); err != nil {
		b.reply(ctx, m, "couldn't answer: "+err.Error())
		return
	}
	b.reply(ctx, m, "✅ answered — relayed to the worker")
}

// cmdDispatch spawns a worker: "/dispatch <repo> <task…>".
func (b *Bot) cmdDispatch(ctx context.Context, m *Message, arg string) string {
	arg = strings.TrimSpace(arg)
	// optional leading "--vm <name>" chooses which VM the agent runs on
	vm := ""
	if strings.HasPrefix(arg, "--vm ") {
		rest := strings.TrimSpace(strings.TrimPrefix(arg, "--vm "))
		vm, arg, _ = strings.Cut(rest, " ")
		arg = strings.TrimSpace(arg)
	}
	repo, task, ok := strings.Cut(arg, " ")
	if !ok || strings.TrimSpace(repo) == "" || strings.TrimSpace(task) == "" {
		return "usage: /dispatch [--vm <name>] <repo> <task>\ne.g. /dispatch /srv/git/app.git add a health endpoint"
	}
	// Issue model: /dispatch inside an existing issue's TOPIC adds an aspect to
	// THAT issue (same session, own worktree/VM/permissions); in General it starts
	// a new issue with its own topic.
	into := ""
	if m.MessageThreadID != 0 {
		if s, ok := b.sessionByTopic(m.MessageThreadID); ok {
			into = s.ID
		}
	}
	wid, sid, err := b.actions.Dispatch(ctx, strings.TrimSpace(repo), strings.TrimSpace(task), vm, into)
	if err != nil {
		return "dispatch failed: " + err.Error()
	}
	// Announce the running target — which VM + herdr workspace/pane — so it's never
	// a mystery where a worker landed (fleet visibility), plus how to resume in CLI.
	detail := "repo: " + repo
	if w, err := b.store.GetWorker(wid); err == nil {
		detail += "\n▶ running on: " + vmLabel(w.VM)
		if w.Workspace != "" {
			detail += "\nherdr workspace: " + w.Workspace
			if w.AgentRef != "" {
				detail += " · pane " + w.AgentRef
			}
		}
		detail += "\nresume in CLI: " + resumeHint(w)
	}
	// Adding an aspect INTO an existing issue: the command came from that issue's
	// topic, so the confirmation lands there alongside the issue's other work.
	if into != "" {
		return fmt.Sprintf("➕ added aspect to this issue — worker %s (session %s)\n%s", short(wid), short(sid), detail)
	}
	// New issue: open its topic NOW rather than waiting for the first escalation —
	// "a topic owns the issue", so the issue must have its home the moment it starts
	// (the operator asked to spawn into a new topic). Post the starter card there +
	// pin the status card; reply in the origin channel with a pointer.
	tid, terr := b.ensureTopic(ctx, sid)
	if terr != nil {
		return fmt.Sprintf("🚀 started issue — worker %s (session %s)\n%s\n(couldn't open its topic: %v)", short(wid), short(sid), detail, terr)
	}
	b.refreshStatus(ctx, sid, tid)
	_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: tid,
		Text: fmt.Sprintf("🚀 issue started — worker %s\ntask: %s\n%s", short(wid), truncate(task, 200), detail)})
	return fmt.Sprintf("🚀 started issue %s — opened its topic ⤴ (worker %s)", short(sid), short(wid))
}

// vmLabel is the human name of a worker's VM ("" = the local herdr on this box).
func vmLabel(vm string) string {
	if vm == "" {
		return "local (this box)"
	}
	return vm
}

// resumeHint tells the operator how to open the worker's herdr pane from a shell.
func resumeHint(w core.Worker) string {
	base := "herdr"
	if w.VM != "" {
		base = "herdr --remote " + w.VM
	}
	if w.Workspace != "" {
		return base + "  → focus workspace " + w.Workspace
	}
	return base
}

// cmdKill terminates a worker resolved by id prefix.
func (b *Bot) cmdKill(ctx context.Context, arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "usage: /kill <worker-id> (a prefix is fine)"
	}
	w, err := b.resolveWorker(arg)
	if err != nil {
		return err.Error()
	}
	if err := b.actions.Kill(ctx, w.ID); err != nil {
		return "kill failed: " + err.Error()
	}
	return "🛑 killed worker " + short(w.ID)
}

// cmdDiff posts a worker's redacted diff into the topic.
func (b *Bot) cmdDiff(ctx context.Context, m *Message, arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		b.reply(ctx, m, "usage: /diff <worker-id> (a prefix is fine)")
		return
	}
	w, err := b.resolveWorker(arg)
	if err != nil {
		b.reply(ctx, m, err.Error())
		return
	}
	patch, err := b.actions.WorkerDiff(ctx, w.ID)
	if err != nil {
		b.reply(ctx, m, "diff error: "+err.Error())
		return
	}
	if b.redact != nil {
		patch, _ = b.redact.Scrub(patch)
	}
	if strings.TrimSpace(patch) == "" {
		patch = "(no diff — base == head)"
	}
	b.reply(ctx, m, truncate("diff — "+short(w.ID)+"\n\n"+patch, tgMessageCap))
}

func (b *Bot) renderSessions() string {
	sessions, _ := b.store.ListSessions(core.SessionFilter{})
	var b2 strings.Builder
	n := 0
	for _, s := range sessions {
		if s.Kind == core.SessionKindPool || s.Status == core.SessionDone || s.Status == core.SessionArchived {
			continue
		}
		n++
		label := firstNonEmpty(s.Slug, s.Title, truncate(s.Goal, 40), s.ID)
		topic := "—"
		if s.TGTopicID != nil && *s.TGTopicID != 0 {
			topic = "topic set"
		}
		fmt.Fprintf(&b2, "• %s  [%s]  %s\n", label, s.Status, topic)
	}
	if n == 0 {
		return "no active sessions — /dispatch <repo> <task> to start one"
	}
	return fmt.Sprintf("sessions (%d):\n%s", n, strings.TrimRight(b2.String(), "\n"))
}

func (b *Bot) renderWorkers() string {
	workers, _ := b.store.ListWorkers(core.WorkerFilter{})
	var active []core.Worker
	for _, w := range workers {
		if !w.State.Terminal() {
			active = append(active, w)
		}
	}
	if len(active) == 0 {
		return "no active workers"
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	var b2 strings.Builder
	fmt.Fprintf(&b2, "workers (%d active):\n", len(active))
	for _, w := range active {
		vm := "local"
		if w.VM != "" {
			vm = w.VM
		}
		pane := w.AgentRef
		if pane == "" {
			pane = "—"
		}
		fmt.Fprintf(&b2, "• %s [%s] vm=%s pane=%s  %s\n", short(w.ID), w.State, vm, pane, truncate(w.Task, 40))
	}
	return strings.TrimRight(b2.String(), "\n")
}

// renderVMs lists the attached fleet — the deterministic answer to "how many
// VMs", not a brain guess.
func (b *Bot) renderVMs() string {
	if len(b.vms) == 0 {
		return "no VMs configured"
	}
	var b2 strings.Builder
	fmt.Fprintf(&b2, "attached VMs (%d):\n", len(b.vms))
	for _, v := range b.vms {
		fmt.Fprintf(&b2, "• %s\n", v)
	}
	return strings.TrimRight(b2.String(), "\n")
}

// cmdScan lists the live herdr agent sessions across the fleet, marking which
// arco already tracks and how to adopt the rest.
func (b *Bot) cmdScan(ctx context.Context) string {
	agents, err := b.actions.Scan(ctx)
	if err != nil {
		return "scan failed: " + err.Error()
	}
	if len(agents) == 0 {
		return "no herdr agent sessions found on the fleet"
	}
	live, done, adoptable := 0, 0, 0
	for _, a := range agents {
		if a.Alive {
			live++
		} else {
			done++
		}
		if a.Alive && !a.Tracked {
			adoptable++
		}
	}
	var sb strings.Builder
	// Count matches what the operator sees in herdr (all panes), with live vs done
	// broken out — a herdr "done" agent is a finished pane, still listed but inert.
	fmt.Fprintf(&sb, "herdr agent sessions (%d — %d live, %d done):\n", len(agents), live, done)
	for _, a := range agents {
		mark := "🆓 untracked"
		switch {
		case !a.Alive:
			mark = "🏁 finished (pane lingering)"
		case a.Tracked:
			mark = "✅ tracked " + short(a.WorkerID)
		}
		fmt.Fprintf(&sb, "\n• %s [%s] on %s — %s\n", a.Kind, a.Status, vmLabel(a.VM), mark)
		if a.Title != "" {
			fmt.Fprintf(&sb, "  %s\n", truncate(a.Title, 60))
		}
		if a.Cwd != "" {
			fmt.Fprintf(&sb, "  cwd: %s\n", a.Cwd)
		}
		fmt.Fprintf(&sb, "  pane: %s", a.Ref)
		if a.SessionID != "" {
			fmt.Fprintf(&sb, " · session %s", short(a.SessionID))
		}
		sb.WriteString("\n")
	}
	if adoptable > 0 {
		sb.WriteString("\nadopt with /adopt <pane> (or /adopt all to track every live untracked one)")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// cmdAdopt registers untracked herdr sessions as monitor-only arco workers.
// No arg (or "all") adopts every untracked live agent; otherwise it adopts the
// one whose pane/workspace/session-id matches the fragment.
func (b *Bot) cmdAdopt(ctx context.Context, arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" || strings.EqualFold(arg, "all") {
		agents, err := b.actions.Scan(ctx)
		if err != nil {
			return "adopt failed (scan): " + err.Error()
		}
		var refs []string
		for _, a := range agents {
			if a.Alive && !a.Tracked { // skip finished (done) panes — nothing to monitor
				refs = append(refs, a.Ref)
			}
		}
		if len(refs) == 0 {
			return "nothing to adopt — every live agent is already tracked (/scan to see)"
		}
		var sb strings.Builder
		for _, ref := range refs {
			wid, sid, err := b.actions.Adopt(ctx, ref)
			if err != nil {
				fmt.Fprintf(&sb, "• %s — skipped: %s\n", ref, err.Error())
				continue
			}
			fmt.Fprintf(&sb, "• %s — 👁 adopted as worker %s (session %s, monitor-only)\n", ref, short(wid), short(sid))
		}
		return strings.TrimRight(sb.String(), "\n")
	}
	wid, sid, err := b.actions.Adopt(ctx, arg)
	if err != nil {
		return "adopt failed: " + err.Error()
	}
	return fmt.Sprintf("👁 adopted %s as worker %s (session %s)\nmonitor-only (manual mode): arco tracks liveness + relays, but didn't launch it so it can't enforce grants.", arg, short(wid), short(sid))
}

// chatPrompt frames a conversational brain call with light fleet context. It
// includes the LIVE herdr agent sessions (from /scan) so a natural-language
// question like "how many claude sessions are running?" is answered from real
// fleet state — not just arco's own ledger (which only counts workers arco
// launched, and would wrongly say "0" while other herdr sessions run).
func (b *Bot) chatPrompt(ctx context.Context, text string) string {
	workers, _ := b.store.ListWorkers(core.WorkerFilter{})
	active, pending := 0, 0
	for _, w := range workers {
		if !w.State.Terminal() {
			active++
		}
	}
	if escs, err := b.store.ListEscalations(core.EscalationFilter{Status: "pending"}); err == nil {
		pending = len(escs)
	}
	vms := "none configured"
	if len(b.vms) > 0 {
		vms = fmt.Sprintf("%d — %s", len(b.vms), strings.Join(b.vms, "; "))
	}
	// Wording lives in the editable prompts/defaults/chat.tmpl; code supplies the
	// facts. On a (build-time) template error, fall back so chat never hard-fails.
	out, err := prompts.Render("chat.tmpl", map[string]any{
		"VMs": vms, "Active": active, "Pending": pending,
		"HerdrSessions": b.herdrSessionFacts(ctx), "Message": text,
	})
	if err != nil {
		return "You are arco, a fleet supervisor. Be concise. Operator says: " + text
	}
	return out
}

// herdrSessionFacts summarizes the LIVE herdr agent sessions on the fleet (the
// /scan data) for the chat context: kind, status, cwd, and whether arco tracks
// it. This is what lets the brain answer "what claude sessions are running?"
// including ones arco didn't launch (this very session, a sysadmin session, …).
func (b *Bot) herdrSessionFacts(ctx context.Context) string {
	agents, err := b.actions.Scan(ctx)
	if err != nil {
		return "unavailable (herdr scan failed: " + err.Error() + ")"
	}
	if len(agents) == 0 {
		return "none detected"
	}
	live := 0
	for _, a := range agents {
		if a.Alive {
			live++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d total (%d live, %d finished/done) —", len(agents), live, len(agents)-live)
	for _, a := range agents {
		track := "untracked by arco"
		if a.Tracked {
			track = "tracked as " + short(a.WorkerID)
		}
		fmt.Fprintf(&sb, " • %s [%s] on %s cwd=%s (%s)", a.Kind, a.Status, vmLabel(a.VM), a.Cwd, track)
	}
	return sb.String()
}

// resolveWorker finds the single worker whose id contains the given fragment
// (case-insensitive) — so the last-8 handle shown by /workers works, as does any
// distinctive slice. Ambiguous or missing is an error message.
func (b *Bot) resolveWorker(fragment string) (core.Worker, error) {
	up := strings.ToUpper(fragment)
	workers, err := b.store.ListWorkers(core.WorkerFilter{})
	if err != nil {
		return core.Worker{}, fmt.Errorf("lookup failed: %w", err)
	}
	var matches []core.Worker
	for _, w := range workers {
		if strings.Contains(strings.ToUpper(w.ID), up) {
			matches = append(matches, w)
		}
	}
	switch len(matches) {
	case 0:
		return core.Worker{}, fmt.Errorf("no worker matches %q", fragment)
	case 1:
		return matches[0], nil
	default:
		return core.Worker{}, fmt.Errorf("%q matches %d workers — use more of the id", fragment, len(matches))
	}
}

// pendingQuestions returns a session's pending QUESTION escalations.
func (b *Bot) pendingQuestions(sessionID string) []core.Escalation {
	escs, err := b.store.ListEscalations(core.EscalationFilter{SessionID: sessionID, Status: "pending"})
	if err != nil {
		return nil
	}
	var out []core.Escalation
	for _, e := range escs {
		if e.Kind == "question" {
			out = append(out, e)
		}
	}
	return out
}

// reply posts text into the message's topic.
func (b *Bot) reply(ctx context.Context, m *Message, text string) {
	_, _ = b.api.SendMessage(ctx, SendMessageReq{ChatID: b.groupID, MessageThreadID: m.MessageThreadID, Text: text})
}

// short is the display form of a ULID: last 8 chars (enough to eyeball/prefix).
func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}
