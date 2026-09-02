package telegram

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/schedule"
)

// scheduleUsage is shown on a malformed /schedule.
const scheduleUsage = `usage: /schedule <when> :: <prompt>
  when = 30m | 2h | 1d | 1w | a 5-field cron (e.g. 0 8 * * *)
  e.g. /schedule 30m :: check the fleet for stuck workers and report
       /schedule 0 8 * * * :: brief me on open issues
manage: /schedule list | pause <id> | resume <id> | remove <id>`

// cmdSchedule creates and manages recurring scheduled tasks. Creation is the
// operator's own explicit command (no confirm gate — the brain-proposed NL path
// is what's confirm-gated). Each task runs as an agentic Converse in its own
// topic + memory; the scheduler loop fires it when due.
func (b *Bot) cmdSchedule(ctx context.Context, arg string) string {
	if b.tasks == nil {
		return "scheduled tasks aren't available on this deployment (no store wired)."
	}
	arg = strings.TrimSpace(arg)
	head, rest, _ := strings.Cut(arg, " ")
	switch strings.ToLower(head) {
	case "", "list", "ls":
		return b.scheduleList()
	case "pause", "disable":
		return b.scheduleSetEnabled(ctx, strings.TrimSpace(rest), false)
	case "resume", "enable":
		return b.scheduleSetEnabled(ctx, strings.TrimSpace(rest), true)
	case "remove", "rm", "delete", "del":
		return b.scheduleRemove(ctx, strings.TrimSpace(rest))
	}

	// Otherwise: a create — "<when> :: <prompt>".
	spec, prompt, ok := strings.Cut(arg, "::")
	if !ok {
		return scheduleUsage
	}
	spec, prompt = strings.TrimSpace(spec), strings.TrimSpace(prompt)
	if prompt == "" {
		return scheduleUsage
	}
	sp, err := schedule.Parse(spec)
	if err != nil {
		return "couldn't read the schedule: " + err.Error() + "\n\n" + scheduleUsage
	}
	name := deriveTaskName(prompt)
	next := sp.Next(time.Now())
	task, err := b.tasks.CreateTask(ctx, name, sp.Canonical(), prompt, next)
	if err != nil {
		return "couldn't create the task: " + err.Error()
	}
	return fmt.Sprintf("⏰ scheduled %q (%s)\n   id %s · next run %s\nit runs unattended in its own topic — /schedule list to manage.",
		name, sp.Canonical(), shortTaskID(task.ID), next.Format("Mon Jan 2 15:04"))
}

// scheduleList renders all tasks, newest first.
func (b *Bot) scheduleList() string {
	tasks, err := b.tasks.ListTasks()
	if err != nil {
		return "couldn't list tasks: " + err.Error()
	}
	if len(tasks) == 0 {
		return "no scheduled tasks yet. create one with:\n" + scheduleUsage
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	var sb strings.Builder
	sb.WriteString("⏰ scheduled tasks:\n")
	for _, t := range tasks {
		state := "on"
		if !t.Enabled {
			state = "paused"
		}
		fmt.Fprintf(&sb, "\n%s  %s  [%s]\n   %s · next %s",
			shortTaskID(t.ID), t.Name, state, t.Schedule, taskWhen(t.NextRun, t.Enabled))
		if t.LastStatus != "" {
			fmt.Fprintf(&sb, "\n   last: %s", t.LastStatus)
			if t.LastResult != "" {
				fmt.Fprintf(&sb, " — %s", truncate(oneLine(t.LastResult), 80))
			}
		}
	}
	return sb.String()
}

func (b *Bot) scheduleSetEnabled(ctx context.Context, frag string, on bool) string {
	t, err := b.resolveTask(frag)
	if err != nil {
		return err.Error()
	}
	if err := b.tasks.SetTaskEnabled(ctx, t.ID, on); err != nil {
		return "couldn't update the task: " + err.Error()
	}
	if on {
		return fmt.Sprintf("▶️ resumed %q", t.Name)
	}
	return fmt.Sprintf("⏸️ paused %q — it won't fire until resumed", t.Name)
}

func (b *Bot) scheduleRemove(ctx context.Context, frag string) string {
	t, err := b.resolveTask(frag)
	if err != nil {
		return err.Error()
	}
	if err := b.tasks.DeleteTask(ctx, t.ID); err != nil {
		return "couldn't remove the task: " + err.Error()
	}
	return fmt.Sprintf("🗑️ removed %q (its topic stays for the record)", t.Name)
}

// resolveTask finds one task by an id fragment (the short id, or any unique
// suffix/substring of the full id). It refuses an ambiguous or empty match rather
// than guess — the same posture as worker resolution.
func (b *Bot) resolveTask(frag string) (core.ScheduledTask, error) {
	frag = strings.TrimSpace(frag)
	if frag == "" {
		return core.ScheduledTask{}, fmt.Errorf("which task? give an id from /schedule list")
	}
	tasks, err := b.tasks.ListTasks()
	if err != nil {
		return core.ScheduledTask{}, fmt.Errorf("couldn't list tasks: %w", err)
	}
	var hits []core.ScheduledTask
	for _, t := range tasks {
		if strings.EqualFold(t.ID, frag) || strings.Contains(strings.ToLower(t.ID), strings.ToLower(frag)) {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return core.ScheduledTask{}, fmt.Errorf("no task matches %q — /schedule list", frag)
	default:
		return core.ScheduledTask{}, fmt.Errorf("%q matches %d tasks — use a longer id", frag, len(hits))
	}
}

// shortTaskID is the last 6 chars of a ULID — enough to disambiguate by hand.
func shortTaskID(id string) string {
	if len(id) <= 6 {
		return id
	}
	return id[len(id)-6:]
}

// deriveTaskName makes a short human name from the prompt's first words.
func deriveTaskName(prompt string) string {
	words := strings.Fields(oneLine(prompt))
	if len(words) > 6 {
		words = words[:6]
	}
	name := strings.Join(words, " ")
	if len(name) > 48 {
		name = strings.TrimSpace(name[:48])
	}
	if name == "" {
		return "task"
	}
	return name
}

// taskWhen renders a next-run time relative to now (paused tasks show "paused").
func taskWhen(next time.Time, enabled bool) string {
	if !enabled {
		return "paused"
	}
	if next.IsZero() {
		return "—"
	}
	d := time.Until(next)
	switch {
	case d < 0:
		return "due now"
	case d < time.Minute:
		return "in <1m"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return next.Format("Mon Jan 2 15:04")
	}
}

// oneLine collapses whitespace/newlines to single spaces for compact rendering.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
