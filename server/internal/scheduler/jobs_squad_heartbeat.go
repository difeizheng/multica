package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// JobNameSquadHeartbeatInspect is the canonical job name written to
// sys_cron_executions audit rows. Stable across releases — renaming it
// would orphan historic rows.
const JobNameSquadHeartbeatInspect = "squad_heartbeat_inspect"

// squadHeartbeatFollowupReason is the handoff note attached to every leader
// task produced by the periodic heartbeat. The leader sees this in its opening
// context. A heartbeat does NOT mean "all is well" — it tells the leader to
// verify actual member progress and, if members are idle while the issue is
// still open, treat that as a stall and re-dispatch (action), not no_action.
// Kept English to match the squadOperatingProtocol convention (protocol text is
// English-only); the team-facing reason the leader records is governed by the
// protocol's language hard rule, not by this note.
const squadHeartbeatFollowupReason = "Squad heartbeat: periodic check-in. Do NOT assume everything is fine. Verify the squad's actual progress: check when each member LAST produced work (a completed task, a comment, a status or code change). If a member is currently working or produced work within the last working step, record no_action. But if no member is currently working and no member has produced anything recently while the issue is still open, the work has stalled — re-dispatch the next step to the right member now (this is an action, not a no_action), exactly as you would for a stall. Never record no_action for an open issue whose members are all idle."

// squadHeartbeatStore is the narrow read contract the heartbeat inspector
// needs from the DB layer. *db.Queries satisfies it structurally. Defined here
// so the handler is unit-testable without a live Postgres, mirroring
// squadHealthStore in jobs_squad_health.go.
type squadHeartbeatStore interface {
	ListSquadHeartbeatDueIssues(ctx context.Context) ([]db.ListSquadHeartbeatDueIssuesRow, error)
	GetIssue(ctx context.Context, id pgtype.UUID) (db.Issue, error)
}

// SquadHeartbeatInspectJob returns the JobSpec that periodically re-inspects
// every open squad-assigned issue on a fixed cadence, regardless of whether the
// issue is stalled. Whereas SquadHealthInspectJob only wakes leaders about
// stalled issues, this job guarantees a visible, regular "Inspections" rhythm:
// each open squad issue gets a leader re-evaluation once its squad's configured
// heartbeat_interval_minutes has elapsed since the last evaluation.
//
// The job ticks at a 5-minute base cadence (the minimum allowed per-squad
// interval); the per-issue "is it due?" decision is made inside the SQL query
// (ListSquadHeartbeatDueIssues), which compares each squad's interval against
// its issues' last squad_leader_evaluated activity. Like the stall inspector,
// the actual leader wake-up is deduped again inside
// EnqueueSquadLeaderFollowUp, so a race between this tick, the stall tick, and
// the terminal-state hook cannot produce a duplicate enqueue.
func SquadHeartbeatInspectJob(queries squadHeartbeatStore, follower SquadFollowUpper) JobSpec {
	return JobSpec{
		Name:              JobNameSquadHeartbeatInspect,
		Cadence:           5 * time.Minute,
		ScheduleDelay:     5 * time.Minute,
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
		Handler: makeSquadHeartbeatInspectHandler(queries, follower),
	}
}

func makeSquadHeartbeatInspectHandler(queries squadHeartbeatStore, follower SquadFollowUpper) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		rows, err := queries.ListSquadHeartbeatDueIssues(ctx)
		if err != nil {
			return HandlerResult{}, fmt.Errorf("list heartbeat-due squad issues: %w", err)
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
				slog.Warn("squad heartbeat: load issue failed",
					"squad_id", util.UUIDToString(row.SquadID),
					"issue_id", util.UUIDToString(row.IssueID),
					"error", err)
				continue
			}
			// Pass an invalid triggering agent: the heartbeat is a scheduler
			// tick, not a member action, so the self-trigger guard inside
			// EnqueueSquadLeaderFollowUp must not reject this wake-up.
			ok, ferr := follower.EnqueueSquadLeaderFollowUp(ctx, issue, pgtype.UUID{}, squadHeartbeatFollowupReason)
			if ferr != nil {
				slog.Warn("squad heartbeat: leader follow-up failed",
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
