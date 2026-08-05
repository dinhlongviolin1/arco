package ledger

import (
	"database/sql"

	"github.com/dinhlongviolin1/arco/internal/core"
)

const workerCols = `id,title,vm,workspace,worktree,base_commit,head_commit,program,agent_kind,boot_id,` +
	`pid,pid_start_time,state,rev,stall_count,owner_session,session_perm_rev,permissions_hash,` +
	`compiled_config_path,task,run_reason,parent_worker_id,delegation_depth,role,summary,` +
	`last_seen_at,last_event_at,pooled_at,created_at`

const sessionCols = `id,slug,title,goal,status,kind,parent_session,rev,perm_rev,mem_rev,permissions,` +
	`context_summary,context_rev,facts,progress,repo,default_vm,pinned,notify_level,tg_topic_id,` +
	`tg_status_msg_id,stall_count,last_activity_at,created_at,closed_at`

const eventCols = `id,source,source_event_id,source_event_hash,worker_id,session_id,kind,actor,` +
	`causation_event_id,correlation_id,payload,occurred_at,recorded_at`

type scanner interface{ Scan(dest ...any) error }

func scanWorker(sc scanner) (core.Worker, error) {
	var w core.Worker
	var pid sql.NullInt64
	var parent, pooled sql.NullString
	err := sc.Scan(
		&w.ID, &w.Title, &w.VM, &w.Workspace, &w.Worktree, &w.BaseCommit, &w.HeadCommit, &w.Program,
		&w.AgentKind, &w.BootID, &pid, &w.PIDStartTime, &w.State, &w.Rev, &w.StallCount, &w.OwnerSession,
		&w.SessionPermRev, &w.PermissionsHash, &w.CompiledConfig, &w.Task, &w.RunReason, &parent,
		&w.DelegationDepth, &w.Role, &w.Summary, &w.LastSeenAt, &w.LastEventAt, &pooled, &w.CreatedAt,
	)
	if err != nil {
		return core.Worker{}, err
	}
	if pid.Valid {
		v := int(pid.Int64)
		w.PID = &v
	}
	w.ParentWorkerID = parent.String
	w.PooledAt = pooled.String
	return w, nil
}

func scanSession(sc scanner) (core.Session, error) {
	var s core.Session
	var slug, parent, closed sql.NullString
	var topic, statusMsg sql.NullInt64
	var pinned int
	err := sc.Scan(
		&s.ID, &slug, &s.Title, &s.Goal, &s.Status, &s.Kind, &parent, &s.Rev, &s.PermRev, &s.MemRev,
		&s.Permissions, &s.ContextSummary, &s.ContextRev, &s.Facts, &s.Progress, &s.Repo, &s.DefaultVM,
		&pinned, &s.NotifyLevel, &topic, &statusMsg, &s.StallCount, &s.LastActivityAt, &s.CreatedAt, &closed,
	)
	if err != nil {
		return core.Session{}, err
	}
	s.Slug = slug.String
	s.ParentSession = parent.String
	s.ClosedAt = closed.String
	s.Pinned = pinned != 0
	if topic.Valid {
		s.TGTopicID = &topic.Int64
	}
	if statusMsg.Valid {
		s.TGStatusMsgID = &statusMsg.Int64
	}
	return s, nil
}

func scanEvent(sc scanner) (core.Event, error) {
	var e core.Event
	var srcID, srcHash, worker, session, corr sql.NullString
	var causation sql.NullInt64
	err := sc.Scan(
		&e.ID, &e.Source, &srcID, &srcHash, &worker, &session, &e.Kind, &e.Actor, &causation, &corr,
		&e.Payload, &e.OccurredAt, &e.RecordedAt,
	)
	if err != nil {
		return core.Event{}, err
	}
	e.SourceEventID = srcID.String
	e.SourceEventHash = srcHash.String
	e.WorkerID = worker.String
	e.SessionID = session.String
	e.CorrelationID = corr.String
	if causation.Valid {
		e.CausationEventID = &causation.Int64
	}
	return e, nil
}

// nullStr maps "" → NULL so UNIQUE(source, source_event_id) treats internal
// events (no source id) as distinct rows rather than colliding on "".
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
