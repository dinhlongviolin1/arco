package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
)

const scheduledCols = `id, name, schedule, prompt, session_id, enabled, next_run, last_run, last_status, last_result, created_at`

// maxDueTasks bounds one scheduler pass — a safety cap, never hit in practice.
const maxDueTasks = 100

// CreateScheduledTask inserts a recurring/planned task. The prompt is scrubbed at
// this write chokepoint (like worker free-text) so a secret in a task instruction
// never persists.
func (t *txn) CreateScheduledTask(st core.ScheduledTask) error {
	if st.ID == "" || st.Name == "" || st.Schedule == "" || st.SessionID == "" {
		return fmt.Errorf("ledger: CreateScheduledTask requires id, name, schedule, session_id")
	}
	prompt := st.Prompt
	if t.scrub != nil {
		prompt, _ = t.scrub.Scrub(prompt)
	}
	created := st.CreatedAt
	if created.IsZero() {
		created = t.clockNow()
	}
	_, err := t.q.ExecContext(context.Background(),
		`INSERT INTO scheduled_tasks(id, name, schedule, prompt, session_id, enabled, next_run, last_run, last_status, last_result, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		st.ID, st.Name, st.Schedule, prompt, st.SessionID, boolToInt(st.Enabled),
		fmtTime(st.NextRun), nullTime(st.LastRun), st.LastStatus, st.LastResult, fmtTime(created))
	return err
}

// RecordScheduledRun stamps a completed run: last_run, the recomputed next_run,
// status and a short result line.
func (t *txn) RecordScheduledRun(id string, lastRun, nextRun time.Time, status, result string) error {
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE scheduled_tasks SET last_run=?, next_run=?, last_status=?, last_result=? WHERE id=?`,
		fmtTime(lastRun), fmtTime(nextRun), status, result, id)
	return affectedOne(res, err, id)
}

func (t *txn) SetScheduledTaskEnabled(id string, enabled bool) error {
	res, err := t.q.ExecContext(context.Background(),
		`UPDATE scheduled_tasks SET enabled=? WHERE id=?`, boolToInt(enabled), id)
	return affectedOne(res, err, id)
}

func (t *txn) DeleteScheduledTask(id string) error {
	res, err := t.q.ExecContext(context.Background(), `DELETE FROM scheduled_tasks WHERE id=?`, id)
	return affectedOne(res, err, id)
}

// DueScheduledTasks returns enabled tasks whose next_run is at or before now. Uses
// the house pattern (coarse whole-second SUPERSET prefilter + exact in-Go cutoff)
// since RFC3339Nano is not lexically chronological.
func (r *reader) DueScheduledTasks(now time.Time, limit int) ([]core.ScheduledTask, error) {
	if limit <= 0 || limit > maxDueTasks {
		limit = maxDueTasks
	}
	bound := now.Add(time.Second).UTC().Format(secondFmt) // superset: admits up to ~1s early, never drops a due task
	rows, err := r.q.QueryContext(context.Background(),
		`SELECT `+scheduledCols+` FROM scheduled_tasks WHERE enabled=1 AND next_run <= ? ORDER BY next_run ASC LIMIT ?`,
		bound, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all, err := scanScheduled(rows)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, tk := range all {
		if !tk.NextRun.After(now) { // exact cutoff
			out = append(out, tk)
		}
	}
	return out, nil
}

func (r *reader) ListScheduledTasks() ([]core.ScheduledTask, error) {
	rows, err := r.q.QueryContext(context.Background(),
		`SELECT `+scheduledCols+` FROM scheduled_tasks ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanScheduled(rows)
}

func (r *reader) GetScheduledTask(id string) (core.ScheduledTask, error) {
	rows, err := r.q.QueryContext(context.Background(),
		`SELECT `+scheduledCols+` FROM scheduled_tasks WHERE id=?`, id)
	if err != nil {
		return core.ScheduledTask{}, err
	}
	defer rows.Close()
	tasks, err := scanScheduled(rows)
	if err != nil {
		return core.ScheduledTask{}, err
	}
	if len(tasks) == 0 {
		return core.ScheduledTask{}, core.ErrNotFound
	}
	return tasks[0], nil
}

func scanScheduled(rows rowScanner) ([]core.ScheduledTask, error) {
	var out []core.ScheduledTask
	for rows.Next() {
		var (
			st               core.ScheduledTask
			enabled          int
			nextRun, created string
			lastRun          sql.NullString
		)
		if err := rows.Scan(&st.ID, &st.Name, &st.Schedule, &st.Prompt, &st.SessionID,
			&enabled, &nextRun, &lastRun, &st.LastStatus, &st.LastResult, &created); err != nil {
			return nil, err
		}
		st.Enabled = enabled != 0
		st.NextRun, _ = time.Parse(time.RFC3339Nano, nextRun)
		st.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if lastRun.Valid && lastRun.String != "" {
			st.LastRun, _ = time.Parse(time.RFC3339Nano, lastRun.String)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// --- small shared helpers ---

func (t *txn) clockNow() time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, t.now())
	return parsed
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return fmtTime(t)
}

func affectedOne(res sql.Result, err error, id string) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("ledger: scheduled task %q not found: %w", id, core.ErrNotFound)
	}
	return nil
}
