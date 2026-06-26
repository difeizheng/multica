package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// EnqueueSquadLeaderFollowUp wakes a squad's leader about a stalled issue by
// enqueuing an is_leader_task=true task bound to that issue. It is the single
// entry point shared by the two squad-health paths:
//
//   - A (terminal-state hook): a member agent's task just entered a terminal
//     state unexpectedly, so we nudge the leader immediately.
//   - B (periodic scheduler inspector): a squad-assigned issue looks stalled
//     (terminal member work, no active member work), so we nudge the leader.
//
// The function is best-effort and idempotent. It returns enqueued=false (with
// a nil error) without touching the queue when:
//   - the issue is not squad-assigned,
//   - the squad is archived or cannot be resolved,
//   - triggeringAgentID is the squad leader (don't wake the leader about its
//     own task),
//   - the leader already has a queued/dispatched task on this issue (dedup).
//
// Any error from the final enqueue is returned; callers in best-effort paths
// should log it and continue rather than failing their own transition.
func (s *TaskService) EnqueueSquadLeaderFollowUp(ctx context.Context, issue db.Issue, triggeringAgentID pgtype.UUID, reason string) (bool, error) {
	// Only squad-assigned issues have a leader to wake.
	if !issue.AssigneeType.Valid || issue.AssigneeType.String != "squad" || !issue.AssigneeID.Valid {
		return false, nil
	}

	squad, err := s.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
		ID:          issue.AssigneeID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("load squad for follow-up: %w", err)
	}

	// Dedup: skip if the leader already has a queued/dispatched task on this
	// issue. This is the same guard the assign/mention paths use, centralized
	// here so A and B cannot race a duplicate enqueue.
	leaderHasPending, err := s.Queries.HasPendingTaskForIssueAndAgent(ctx, db.HasPendingTaskForIssueAndAgentParams{
		IssueID: issue.ID,
		AgentID: squad.LeaderID,
	})
	if err != nil {
		return false, fmt.Errorf("check pending leader task: %w", err)
	}

	if !shouldEnqueueSquadLeaderFollowUp(issue, squad, triggeringAgentID, leaderHasPending) {
		return false, nil
	}

	if _, err := s.EnqueueTaskForSquadLeaderWithHandoff(ctx, issue, squad.LeaderID, reason); err != nil {
		return false, fmt.Errorf("enqueue squad leader follow-up: %w", err)
	}
	slog.Info("squad leader follow-up enqueued",
		"squad_id", util.UUIDToString(squad.ID),
		"leader_id", util.UUIDToString(squad.LeaderID),
		"issue_id", util.UUIDToString(issue.ID),
	)
	return true, nil
}

// shouldEnqueueSquadLeaderFollowUp is the pure decision behind
// EnqueueSquadLeaderFollowUp, extracted so the no-op conditions are
// unit-testable without a database. It returns true only when the squad is
// active, has a leader distinct from the triggering agent, and the leader
// does not already have a queued/dispatched task on the issue.
func shouldEnqueueSquadLeaderFollowUp(issue db.Issue, squad db.Squad, triggeringAgentID pgtype.UUID, leaderHasPendingTask bool) bool {
	if squad.ArchivedAt.Valid {
		return false
	}
	if !squad.LeaderID.Valid {
		return false
	}
	// Don't wake the leader about its own task.
	if triggeringAgentID.Valid && triggeringAgentID.Bytes == squad.LeaderID.Bytes {
		return false
	}
	if leaderHasPendingTask {
		return false
	}
	return true
}

// maybeEnqueueSquadLeaderFollowUp is the best-effort wrapper used by the
// terminal-state hook (A): it loads the task's issue and delegates to
// EnqueueSquadLeaderFollowUp. It never returns an error — a failed follow-up
// is logged and dropped so it can never fail the caller's own transition.
func (s *TaskService) maybeEnqueueSquadLeaderFollowUp(ctx context.Context, task db.AgentTaskQueue, reason string) {
	if !task.IssueID.Valid {
		return
	}
	issue, err := s.Queries.GetIssue(ctx, task.IssueID)
	if err != nil {
		slog.Warn("squad leader follow-up: issue not found",
			"task_id", util.UUIDToString(task.ID),
			"issue_id", util.UUIDToString(task.IssueID),
			"error", err)
		return
	}
	if _, err := s.EnqueueSquadLeaderFollowUp(ctx, issue, task.AgentID, reason); err != nil {
		slog.Warn("squad leader follow-up failed",
			"task_id", util.UUIDToString(task.ID),
			"issue_id", util.UUIDToString(task.IssueID),
			"error", err)
	}
}
