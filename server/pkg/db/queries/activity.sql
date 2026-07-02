-- name: ListActivitiesForIssue :many
-- All activities for an issue in chronological order, capped at $2 (DB safety
-- net to bound the response).
SELECT * FROM activity_log
WHERE issue_id = $1
ORDER BY created_at ASC, id ASC
LIMIT $2;

-- name: GetActivity :one
SELECT * FROM activity_log
WHERE id = $1;

-- name: CreateActivity :one
INSERT INTO activity_log (
    workspace_id, issue_id, actor_type, actor_id, action, details
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: HasSquadLeaderNoActionEvaluationForTask :one
SELECT EXISTS (
  SELECT 1
  FROM activity_log
  WHERE issue_id = @issue_id
    AND actor_type = 'agent'
    AND actor_id = @agent_id
    AND action = 'squad_leader_evaluated'
    AND details->>'outcome' = 'no_action'
    AND details->>'task_id' = @task_id::text
) AS exists;

-- name: CountDispatchedMemberTasksForIssueSubtree :one
-- Counts member tasks spawned on an issue OR its direct children during the
-- half-open window [since, now]. Used by RecordSquadLeaderEvaluation to
-- verify that an `outcome=action` claim actually produced downstream member
-- work (either an @mention dispatch on this issue or a child issue created
-- and assigned to an agent/member). The leader agent itself is excluded so
-- its own coordination runs do not count as a dispatch.
SELECT COUNT(*)::int
FROM agent_task_queue t
WHERE t.created_at >= sqlc.arg(since)
  AND t.agent_id <> sqlc.arg(leader_id)
  AND (
    t.issue_id = sqlc.arg(issue_id)
    OR t.issue_id IN (SELECT id FROM issue WHERE parent_issue_id = sqlc.arg(issue_id))
  );

-- name: ListSquadLeaderEvaluations :many
-- A squad leader's evaluation history: one row per squad_leader_evaluated
-- activity recorded by this squad's leader. Each row is a leader wake-up
-- (whether triggered by the periodic inspector, the terminal-state hook, an
-- @mention, or an assign). Drives the read-only "Inspections" panel.
-- JOIN issue for title/number; LEFT JOIN agent_task_queue (via the task_id
-- stored in details JSON) for the run duration.
SELECT
  a.id,
  a.created_at,
  a.issue_id,
  i.number   AS issue_number,
  i.title    AS issue_title,
  i.status   AS issue_status,
  a.details->>'outcome'  AS outcome,
  a.details->>'reason'   AS reason,
  -- `verified` is only present on outcome=action rows written after the
  -- dispatch-verification feature shipped. NULL for legacy rows and for
  -- no_action/failed outcomes; the handler treats NULL as verified=true so
  -- historical data is not flagged.
  a.details->>'verified' AS verified,
  t.started_at   AS task_started_at,
  t.completed_at AS task_completed_at
FROM activity_log a
LEFT JOIN issue i ON i.id = a.issue_id
LEFT JOIN agent_task_queue t ON t.id::text = a.details->>'task_id'
WHERE a.actor_id = @actor_id
  AND a.action = 'squad_leader_evaluated'
  AND a.details->>'squad_id' = @squad_id::text
ORDER BY a.created_at DESC
LIMIT @limit_rows;

-- name: CountAssigneeChangesByActor :many
-- Count how many times a user assigned each target via assignee_changed activities.
SELECT
  details->>'to_type' as assignee_type,
  details->>'to_id' as assignee_id,
  COUNT(*)::bigint as frequency
FROM activity_log
WHERE workspace_id = $1
  AND actor_id = $2
  AND actor_type = 'member'
  AND action = 'assignee_changed'
  AND details->>'to_type' IS NOT NULL
  AND details->>'to_id' IS NOT NULL
GROUP BY details->>'to_type', details->>'to_id';

-- name: ListSquadHeartbeatDueIssues :many
-- Open squad-assigned issues whose leader is due for a periodic heartbeat
-- wake-up. A row is "due" when the leader has NO queued/dispatched task on the
-- issue AND either the leader has never evaluated it, or the most recent
-- squad_leader_evaluated activity for (issue, leader) is older than the squad's
-- configured heartbeat_interval_minutes.
--
-- The last-evaluation lookup is a grouped LEFT JOIN over activity_log scoped to
-- the last 7 days. 7 days comfortably exceeds the max allowed interval
-- (1440 minutes = 1 day), so the window can never drop the row that determines
-- due-ness, while bounding the grouped scan. Drives the periodic
-- squad_heartbeat_inspect scheduler job.
SELECT
    s.id            AS squad_id,
    s.leader_id     AS leader_id,
    i.id            AS issue_id,
    i.workspace_id  AS workspace_id
FROM squad s
JOIN issue i
       ON i.assignee_type = 'squad'
      AND i.assignee_id = s.id
LEFT JOIN (
    SELECT issue_id, actor_id, MAX(created_at) AS last_eval_at
    FROM activity_log
    WHERE action = 'squad_leader_evaluated'
      AND created_at > now() - interval '7 days'
    GROUP BY issue_id, actor_id
) le ON le.issue_id = i.id AND le.actor_id = s.leader_id
WHERE s.archived_at IS NULL
  AND s.leader_id IS NOT NULL
  AND i.status NOT IN ('done', 'cancelled')
  AND (
      le.last_eval_at IS NULL
      OR le.last_eval_at < now() - (s.heartbeat_interval_minutes * interval '1 minute')
  )
  AND NOT EXISTS (
      SELECT 1 FROM agent_task_queue lt
      WHERE lt.issue_id = i.id
        AND lt.agent_id = s.leader_id
        AND lt.status IN ('queued', 'dispatched')
  );
