package features

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dinhlongviolin1/arco/internal/feature"
	"github.com/dinhlongviolin1/arco/internal/schedule"
)

// ScheduleFunc creates a recurring task and its own session/topic, returning the
// new task id. The daemon wraps its taskStore.CreateTask in this shape so the
// feature needn't import the store. `sched` is already the canonical schedule
// string and `next` its first fire time (both computed here from the raw spec).
type ScheduleFunc func(ctx context.Context, name, sched, prompt string, next time.Time) (taskID string, err error)

// nowFunc lets tests pin "now"; the daemon passes time.Now.
type nowFunc func() time.Time

// Schedule lets the brain PROPOSE a recurring task from natural language ("brief
// me every morning at 8"). Tool-only + BrainAct — creation runs through the same
// confirm gate as any mutating action (the operator approves with a ✅), which is
// exactly the operator's stated policy for creating scheduled work. The richer
// /schedule operator command lives in the telegram layer; this is the brain path.
func Schedule(create ScheduleFunc, now nowFunc) feature.Feature {
	if now == nil {
		now = time.Now
	}
	return feature.Feature{
		Name: "schedule_task",
		Tool: &feature.Tool{
			Name: "schedule_task",
			Desc: `Create a recurring unattended task that runs in its own topic (with tools + memory). ` +
				`Use for requests like "every morning brief me on X" or "check the fleet every 30 minutes". ` +
				`Args: {"schedule":"<when>","prompt":"<what to do each run>","name":"<short label, optional>"}. ` +
				`schedule is an interval (30m, 2h, 1d, 1w) OR a 5-field cron ("0 8 * * *" = daily 08:00).`,
			Schema: json.RawMessage(`{"type":"object","properties":{"schedule":{"type":"string"},"prompt":{"type":"string"},"name":{"type":"string"}},"required":["schedule","prompt"]}`),
			Access: feature.BrainAct,
			Call: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Schedule string `json:"schedule"`
					Prompt   string `json:"prompt"`
					Name     string `json:"name"`
				}
				_ = json.Unmarshal(args, &a)
				spec, prompt := strings.TrimSpace(a.Schedule), strings.TrimSpace(a.Prompt)
				if spec == "" || prompt == "" {
					return `provide {"schedule":"<when>","prompt":"<what to do>"} — e.g. {"schedule":"0 8 * * *","prompt":"brief me on open issues"}`, nil
				}
				sp, err := schedule.Parse(spec)
				if err != nil {
					return "that schedule didn't parse: " + err.Error() + ` — use an interval like "30m"/"2h"/"1d" or a 5-field cron like "0 8 * * *"`, nil
				}
				name := strings.TrimSpace(a.Name)
				if name == "" {
					name = scheduleName(prompt)
				}
				next := sp.Next(now())
				id, err := create(ctx, name, sp.Canonical(), prompt, next)
				if err != nil {
					return "couldn't create the task: " + err.Error(), nil
				}
				return fmt.Sprintf("⏰ scheduled %q (%s) — id %s, next run %s. It runs unattended in its own topic; manage with /schedule.",
					name, sp.Canonical(), short(id), next.Format("Mon Jan 2 15:04")), nil
			},
		},
	}
}

// scheduleName derives a short label from the prompt when the brain omits one.
func scheduleName(prompt string) string {
	words := strings.Fields(strings.Join(strings.Fields(prompt), " "))
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
