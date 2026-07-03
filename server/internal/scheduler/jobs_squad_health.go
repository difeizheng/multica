package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// JobNameSquadHealthInspect is the canonical job name written to
// sys_cron_executions audit rows. Stable across releases — renaming it
// would orphan historic rows.
const JobNameSquadHealthInspect = "squad_health_inspect"

// SquadFollowUpper is the narrow contract this job needs from
// service.TaskService. Defined here so unit tests in this package (and the
// cmd/server integration tests) can stub it without pulling in the rest of
// the service layer. *service.TaskService satisfies it structurally.
type SquadFollowUpper interface {
	// EnqueueSquadLeaderFollowUp enqueues a leader follow-up task for the
	// given issue, deduplicated against any pending leader task. Returns
	// true when a task was actually enqueued. triggeringAgentID is the
	// agent whose stall triggered the wake-up; pass the leader's own ID
	// (or an invalid UUID) to skip the self-trigger guard.
	EnqueueSquadLeaderFollowUp(ctx context.Context, issue db.Issue, triggeringAgentID pgtype.UUID, reason string) (bool, error)
}

// squadHealthFollowupReason is the handoff note attached to every
// leader task produced by the inspector. The leader sees this in its
// opening context and is expected to @mention a member to continue.
// squadHeartbeatLanguageReminder is appended so this English system note does
// not pull the leader's team-facing --reason into English on Chinese issues;
// RecordSquadLeaderEvaluation enforces it as a backstop regardless.
const squadHealthFollowupReason = "Squad health check: this issue appears stalled (a member stopped and no one is actively working). Please review and @mention a member to continue." + squadHeartbeatLanguageReminder

// squadHealthStore is the narrow read contract the inspector needs from the
// DB layer. *db.Queries satisfies it. Defined here so the handler is
// unit-testable without a live Postgres.
type squadHealthStore interface {
	ListStalledSquadIssues(ctx context.Context) ([]db.ListStalledSquadIssuesRow, error)
	GetIssue(ctx context.Context, id pgtype.UUID) (db.Issue, error)
}

// SquadHealthInspectJob returns the JobSpec that periodically inspects
// squads for stalled issues and wakes their leaders. A squad-assigned
// issue is "stalled" when a non-leader member has terminal work on it
// (failed/cancelled/completed), no non-leader member is currently active
// on it, and the leader has no queued/dispatched task on it (see the
// ListStalledSquadIssues query). The leader follow-up itself is deduped
// again inside EnqueueSquadLeaderFollowUp, so a race between this tick
// and the terminal-state hook (A) cannot produce a duplicate enqueue.
//
// Cadence is the minimum interval at which any one squad is re-inspected.
// CatchUpLatestOnly + the scheduler's plan-time flooring guarantee that a
// missed tick collapses into a single catch-up run rather than a replay.
func SquadHealthInspectJob(queries squadHealthStore, follower SquadFollowUpper) JobSpec {
	return JobSpec{
		Name:              JobNameSquadHealthInspect,
		Cadence:           15 * time.Minute,
		ScheduleDelay:     15 * time.Minute,
		CatchUpMode:       CatchUpLatestOnly,
		CatchUpWindow:     24 * time.Hour,
		RunTimeout:        5 * time.Minute,
		StaleTimeout:      10 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       3,
		RetryBackoff: []time.Duration{
			1 * time.Minute,
			5 * time.Minute,
		},
		Scopes:  StaticScopes(ScopeGlobal),
		Handler: makeSquadHealthInspectHandler(queries, follower),
	}
}

func makeSquadHealthInspectHandler(queries squadHealthStore, follower SquadFollowUpper) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		rows, err := queries.ListStalledSquadIssues(ctx)
		if err != nil {
			return HandlerResult{}, fmt.Errorf("list stalled squad issues: %w", err)
		}

		woken := 0
		for _, row := range rows {
			// The issue may have been closed between the candidate scan and
			// now; skip missing rows rather than failing the whole tick.
			issue, err := queries.GetIssue(ctx, row.IssueID)
			if err != nil {
				if isNoRowsErr(err) {
					continue
				}
				slog.Warn("squad health: load issue failed",
					"squad_id", util.UUIDToString(row.SquadID),
					"issue_id", util.UUIDToString(row.IssueID),
					"error", err)
				continue
			}
			// Pass an invalid triggering agent: the scheduler detected the
			// stall on a member (already filtered out of the SQL), not on
			// the leader, so the self-trigger guard inside
			// EnqueueSquadLeaderFollowUp must not reject this wake-up.
			ok, ferr := follower.EnqueueSquadLeaderFollowUp(ctx, issue, pgtype.UUID{}, squadHealthFollowupReason)
			if ferr != nil {
				slog.Warn("squad health: leader follow-up failed",
					"squad_id", util.UUIDToString(row.SquadID),
					"leader_id", util.UUIDToString(row.LeaderID),
					"issue_id", util.UUIDToString(row.IssueID),
					"error", ferr)
				continue
			}
			if ok {
				woken++
			}
			if in.Heartbeat != nil {
				_ = in.Heartbeat(ctx)
			}
		}

		return HandlerResult{
			RowsAffected: int64(woken),
			Result: map[string]any{
				"candidates": len(rows),
				"woken":      woken,
			},
		}, nil
	}
}

// isNoRowsErr reports whether err is a pgx no-rows result, used to skip
// issues that disappeared between the candidate scan and the per-row load.
func isNoRowsErr(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
