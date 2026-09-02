// Package scheduler fires due scheduled tasks on a ticker. It is deliberately
// DECOUPLED from the reconcile sweep/CAS core: it only reads + advances the
// scheduled_tasks table and calls the injected Runner (which runs the task as an
// agentic Converse in its own topic). Runs are sequential in one goroutine, so a
// task can never double-fire (its next_run is advanced before the next tick).
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/schedule"
)

// Runner executes one due task and returns a short result line for the ledger.
type Runner func(ctx context.Context, t core.ScheduledTask) (result string, err error)

const defaultTick = 60 * time.Second

// Scheduler fires due scheduled tasks.
type Scheduler struct {
	Store core.Store
	Run   Runner
	Now   func() time.Time // nil → time.Now
	Tick  time.Duration    // <=0 → 60s
}

// Loop fires due tasks each tick until ctx is cancelled. Blocking — run it in a
// goroutine joined to the daemon's shutdown ctx.
func (s *Scheduler) Loop(ctx context.Context) {
	tick := s.Tick
	if tick <= 0 {
		tick = defaultTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.FireDue(ctx)
		}
	}
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// FireDue runs every task due at now, advancing each to its next fire time. A run
// error is recorded (status="error") but never stops the others; the task stays
// enabled so the operator sees the failure in the topic + /schedule list.
func (s *Scheduler) FireDue(ctx context.Context) {
	now := s.now()
	due, err := s.Store.Reader().DueScheduledTasks(now, 100)
	if err != nil {
		log.Printf("arco: scheduler: due query: %v", err)
		return
	}
	for _, task := range due {
		if ctx.Err() != nil {
			return
		}
		result, runErr := s.Run(ctx, task)
		status := "ok"
		if runErr != nil {
			status = "error"
			if result == "" {
				result = runErr.Error()
			}
			log.Printf("arco: scheduler: task %s (%s) run error: %v", task.ID, task.Name, runErr)
		}
		// Compute next_run from a FRESH clock reading — AFTER the (possibly long)
		// agentic run — not the start-of-pass `now`. A 30m task whose run takes 35m
		// must schedule off completion, or next_run lands in the past and it re-fires
		// every tick. schedule.Next() is always strictly after its argument, so this
		// is future; the loop is a defensive backstop against a pathological schedule.
		after := s.now()
		next := s.nextRun(task, after)
		for !next.After(after) {
			stepped := s.nextRun(task, next)
			if !stepped.After(next) {
				break
			}
			next = stepped
		}
		if err := s.Store.WithTx(ctx, func(tx core.Tx) error {
			return tx.RecordScheduledRun(task.ID, after, next, status, trunc(result, 200))
		}); err != nil {
			log.Printf("arco: scheduler: record run %s: %v", task.ID, err)
		}
	}
}

// nextRun computes the task's next fire time; a malformed schedule backs off an
// hour (the task stays enabled so its error result is visible).
func (s *Scheduler) nextRun(task core.ScheduledTask, now time.Time) time.Time {
	sp, err := schedule.Parse(task.Schedule)
	if err != nil {
		log.Printf("arco: scheduler: task %s has bad schedule %q: %v", task.ID, task.Schedule, err)
		return now.Add(time.Hour)
	}
	return sp.Next(now)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
