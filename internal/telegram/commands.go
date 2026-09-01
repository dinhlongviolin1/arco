package telegram

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
	"github.com/dinhlongviolin1/arco/internal/prompts"
)

const helpText = `arco — drive the fleet from Telegram

act:
  /dispatch <repo> <task>   spawn a worker on <repo> to do <task>
                            (in an issue's topic → adds an aspect to that issue;
                             in General → starts a new issue; --vm <name> to pick a VM)
  /pause  /resume           emergency stop on / off

answer:
  tap the buttons on a decision card, or just TYPE your answer inside
  that worker's topic. Send a photo in a topic to hand it to the worker.

chat:
  type anything else and arco's brain replies.`

// builtinCommands is the single source of truth for the command names the switch
// in handleCommand owns — including the aliases the menu list omits (/new, /start).
// A feature may NOT register any of these; New() rejects a collision at assembly
// (fail loud), and both menu() and /help dedup against it. When a command is
// PORTED to a feature, delete its case AND its entry here in the same change, so
// this set always mirrors the switch.
var builtinCommands = map[string]bool{
	"help": true, "start": true,
	"pause": true, "resume": true, "dispatch": true, "new": true,
}

// helpMessage is the built-in help plus a generated section for registered
// feature commands, so a ported/added feature is documented without editing the
// hardcoded text — the /help surface stays in sync with the registry, exactly
// like the "/" menu does.
func (b *Bot) helpMessage() string {
	if b.reg == nil {
		return helpText
	}
	var extra []feature.Command
	for _, c := range b.reg.Commands() {
		if !builtinCommands[c.Name] { // defensive: assembly already rejected collisions
			extra = append(extra, c)
		}
	}
	if len(extra) == 0 {
		return helpText
	}
	var sb strings.Builder
	sb.WriteString(helpText)
	sb.WriteString("\n\nfeatures:")
	for _, c := range extra {
		sb.WriteString("\n  /" + c.Name)
		if c.Usage != "" {
			sb.WriteString(" " + c.Usage)
		}
		if c.Help != "" {
			sb.WriteString("  — " + c.Help)
		}
	}
	return sb.String()
}

// handleCommand runs a slash-command and replies in the same topic.
func (b *Bot) handleCommand(ctx context.Context, m *Message, text string) {
	fields := strings.Fields(text)
	cmd := strings.ToLower(strings.SplitN(fields[0], "@", 2)[0]) // strip /cmd@botname
	arg := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))

	switch cmd {
	case "/help", "/start":
		b.reply(ctx, m, b.helpMessage())
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
	default:
		// Coexistence seam: a command the switch doesn't own may be served by a
		// registered feature. The switch is consulted first, so no command is ever
		// handled by both, and an empty registry leaves behavior unchanged.
		if b.runFeatureCommand(ctx, m, cmd, arg) {
			return
		}
		b.reply(ctx, m, "unknown command "+cmd+" — /help for the list")
	}
}

// runFeatureCommand dispatches to a registered feature command, returning true if
// one handled it. It resolves the thread's session + the acting operator so the
// feature closure gets its context without reaching back into the bot.
func (b *Bot) runFeatureCommand(ctx context.Context, m *Message, cmd, arg string) bool {
	if b.reg == nil {
		return false
	}
	c, ok := b.reg.Command(cmd)
	if !ok {
		return false
	}
	sessionID := ""
	if m.MessageThreadID != 0 {
		if s, ok := b.sessionByTopic(m.MessageThreadID); ok {
			sessionID = s.ID
		}
	}
	reply, err := c.Run(ctx, feature.CmdInput{
		Arg: arg, ThreadID: m.MessageThreadID, SessionID: sessionID, Actor: actorOf(m),
	})
	if err != nil {
		usage := ""
		if c.Usage != "" {
			usage = "\nusage: /" + c.Name + " " + c.Usage
		}
		b.reply(ctx, m, truncate(b.scrub("⚠️ /"+c.Name+": "+err.Error()+usage), tgMessageCap))
		return true
	}
	// Command chokepoint: SCRUB then TRUNCATE every feature reply before it leaves
	// for Telegram. Scrub-before-truncate matters — a feature may surface a large
	// raw patch/tail (e.g. /diff, /peek), and truncating first could split a secret
	// so the scrubber misses it. Closures don't carry the scrubber, so redaction
	// lives here, uniformly.
	b.reply(ctx, m, truncate(b.scrub(reply), tgMessageCap))
	return true
}

// scrub redacts a string with the bot's scrubber (identity if none is set) — the
// single point every feature-command reply passes through before Telegram.
func (b *Bot) scrub(s string) string {
	if b.redact == nil {
		return s
	}
	out, _ := b.redact.Scrub(s)
	return out
}

// actorOf is the operator identity for authz/audit — the Telegram user id as a
// string, or "" when the sender is unknown.
func actorOf(m *Message) string {
	if m == nil || m.From == nil {
		return ""
	}
	return strconv.FormatInt(m.From.ID, 10)
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
	// (3) conversational reply. When brain tools are registered, chat is AGENTIC:
	// the brain can call read-only tools (scan, …) to check live state itself
	// rather than arco pre-stuffing facts. With no registry/tools it falls back to
	// the one-shot reply. Both keep the short per-thread memory so a follow-up
	// ("peek into it") resolves against prior turns.
	reply, err := b.chatReply(ctx, m.MessageThreadID, text)
	if err != nil {
		b.reply(ctx, m, "🤖 chat unavailable ("+err.Error()+") — try /help for commands")
		return
	}
	b.recordChatTurn(ctx, m.MessageThreadID, text, reply)
	b.reply(ctx, m, reply)
}

// chatReply produces the conversational answer: the agentic tool-loop when brain
// tools are registered, else the legacy one-shot brain call.
func (b *Bot) chatReply(ctx context.Context, threadID int64, text string) (string, error) {
	if b.reg != nil {
		if tools := b.reg.ForBrain(); len(tools) > 0 {
			return b.actions.Converse(ctx, chatSystemPreamble, b.chatContext(threadID, text), b.chatSessionKey(threadID), tools)
		}
	}
	return b.actions.BrainReply(ctx, b.chatPrompt(ctx, threadID, text))
}

// chatSystemPreamble carries the guidance the old chat.tmpl held, adapted for the
// tool-loop: answer only from facts, CALL the scan tool for any fleet-state
// question (the whole reason we stopped pre-stuffing it), and point the operator
// at the exact command when they want to act.
const chatSystemPreamble = `You are arco, a fleet supervisor chatting with the operator over Telegram. Be concise and answer ONLY from facts — never invent workers, sessions, or counts.
For ANY question about what is running, sessions, agents, or fleet state, CALL the scan tool rather than guessing.
If the operator wants to DO something, tell them the exact command: /dispatch <repo> <task> to start a worker, /kill <worker>, /peek <pane>, /adopt [ref], /pause and /resume for the emergency stop.`

// chatContext frames an agentic chat turn: light operator context (attached VMs
// + recent conversation) plus the message. It deliberately OMITS the pre-stuffed
// live herdr scan — the brain fetches that itself via the scan tool when needed,
// which is the whole point of the tool-loop (and avoids a scan on every message).
func (b *Bot) chatContext(threadID int64, text string) string {
	var sb strings.Builder
	if len(b.vms) > 0 {
		sb.WriteString("(attached VMs: " + strings.Join(b.vms, "; ") + ")\n")
	}
	if h := b.chatHistory(threadID); h != "none yet" {
		sb.WriteString("(recent conversation:" + h + "\n)\n")
	}
	sb.WriteString(text)
	return sb.String()
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

// chatPrompt frames a conversational brain call with light fleet context. It
// includes the LIVE herdr agent sessions (from /scan) so a natural-language
// question like "how many claude sessions are running?" is answered from real
// fleet state — not just arco's own ledger (which only counts workers arco
// launched, and would wrongly say "0" while other herdr sessions run).
func (b *Bot) chatPrompt(ctx context.Context, threadID int64, text string) string {
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
		"HerdrSessions": b.herdrSessionFacts(ctx), "History": b.chatHistory(threadID), "Message": text,
	})
	if err != nil {
		return "You are arco, a fleet supervisor. Be concise. Operator says: " + text
	}
	return out
}

// chatSessionKey maps a Telegram thread to the durable-history session key: an
// issue topic's own session, else the console sentinel (General-topic chat has no
// work session but still needs a durable, FK-satisfying key).
func (b *Bot) chatSessionKey(threadID int64) string {
	if threadID != 0 {
		if s, ok := b.sessionByTopic(threadID); ok {
			return s.ID
		}
	}
	return core.ConsoleSessionID
}

// chatHistory renders the recent per-thread conversation for the brain context
// ("none yet" when the thread is fresh) so a follow-up resolves against it. When
// a durable ContextStore is wired it survives restart and is fleet-wide queryable;
// otherwise it falls back to the in-memory per-thread buffer.
func (b *Bot) chatHistory(threadID int64) string {
	if b.cstore != nil {
		msgs, err := b.cstore.RecentMessages(b.chatSessionKey(threadID), 2*maxChatTurns)
		if err != nil || len(msgs) == 0 {
			return "none yet"
		}
		var sb strings.Builder
		for _, m := range msgs {
			lim := 400
			if m.Role == "operator" {
				lim = 300
			}
			fmt.Fprintf(&sb, "\n  %s: %s", m.Role, truncate(m.Content, lim))
		}
		return sb.String()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	turns := b.chatHist[threadID]
	if len(turns) == 0 {
		return "none yet"
	}
	var sb strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&sb, "\n  operator: %s\n  arco: %s", truncate(t.user, 300), truncate(t.bot, 400))
	}
	return sb.String()
}

// recordChatTurn persists one operator↔arco exchange. Durable (per session, via
// the ContextStore — scrubbed at the store's write chokepoint, survives restart)
// when wired; else the in-memory per-thread buffer capped at maxChatTurns.
func (b *Bot) recordChatTurn(ctx context.Context, threadID int64, user, bot string) {
	if b.cstore != nil {
		sid := b.chatSessionKey(threadID)
		// Best-effort: a persist failure must not break the reply, but log it so
		// silently-lost history is observable.
		if err := b.cstore.AppendMessage(ctx, sid, "operator", user); err != nil {
			log.Printf("arco: telegram: persist chat turn (operator): %v", err)
		}
		if err := b.cstore.AppendMessage(ctx, sid, "arco", bot); err != nil {
			log.Printf("arco: telegram: persist chat turn (arco): %v", err)
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	h := append(b.chatHist[threadID], chatTurn{user: user, bot: bot})
	if len(h) > maxChatTurns {
		h = h[len(h)-maxChatTurns:]
	}
	b.chatHist[threadID] = h
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
		fmt.Fprintf(&sb, " • %s [%s] on %s cwd=%s (%s)", a.Kind, a.State, vmLabel(a.VM), a.Cwd, track)
	}
	return sb.String()
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
