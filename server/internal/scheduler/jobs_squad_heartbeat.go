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

// squadHeartbeatFallbackNote is the handoff note used when the per-issue
// telemetry query fails. A telemetry failure must never block the wake-up —
// the leader is still woken and pushed to verify progress itself, exactly as
// it was before telemetry-driven verdicts existed. Kept English to match the
// squadOperatingProtocol convention (protocol/handoff text is English-only;
// the team-facing --reason the leader records is localized separately).
const squadHeartbeatFallbackNote = "Squad heartbeat: periodic check-in (telemetry unavailable). Verify the squad's actual progress yourself: check when each member LAST produced work via the task queue and recent activity. If no member is currently working and nothing has been produced recently while the issue is open, the work has STALLED — re-dispatch the next step now and record `action`, NOT `no_action`. Do not infer members are busy from the in_progress label alone."

// squadHeartbeatTelemetry is the per-issue member-task snapshot the heartbeat
// uses to classify an open squad issue as STALLED vs PROGRESSING
// deterministically, instead of delegating that judgement to the leader LLM.
// The leader has no view into agent_task_queue and, left to infer from the
// in_progress label, systematically records no_action for idle squads — which
// is the bug this struct exists to fix.
type squadHeartbeatTelemetry struct {
	// runningMemberTasks is the count of non-leader tasks on the issue (or
	// its direct children) currently queued/dispatched — i.e. a member is
	// actively working right now.
	runningMemberTasks int32
	// lastMemberTaskActivityAt is the most recent point any non-leader member
	// task on the subtree was known alive (completion, start, or creation,
	// in that order of preference). Valid=false when no non-leader member
	// task has ever touched the subtree.
	lastMemberTaskActivityAt pgtype.Timestamptz
	// heartbeatInterval is the squad's configured cadence; the stall
	// threshold for "no recent activity" is measured in multiples of it.
	heartbeatInterval time.Duration
}

// stalled is the pure, testable classification behind the heartbeat verdict.
// The burden of proof is on "progressing": an issue is stalled unless there is
// positive evidence of current or recent member activity. This deliberately
// inverts the leader LLM's default (which assumes busy from the in_progress
// label) so that silent stalls are caught instead of rubber-stamped.
//
// A member task currently in queued/dispatched state counts as progressing
// regardless of age — a genuinely hung task is the daemon's responsibility to
// reap via its own task timeouts, not the heartbeat's.
func (t squadHeartbeatTelemetry) stalled(now time.Time) bool {
	if t.runningMemberTasks > 0 {
		return false
	}
	if !t.lastMemberTaskActivityAt.Valid {
		return true // no member has ever worked this issue
	}
	return now.Sub(t.lastMemberTaskActivityAt.Time) > t.heartbeatInterval
}

// heartbeatHandoffNote renders the dynamic handoff note for one issue. It
// leads with the verdict and the raw telemetry so the leader LLM can follow
// it mechanically: STALLED ⇒ record `action` and re-dispatch; PROGRESSING ⇒
// record `no_action`. English by convention (see squadHeartbeatFallbackNote).
func heartbeatHandoffNote(t squadHeartbeatTelemetry, now time.Time) string {
	lastActivity := "no member task has ever touched this issue"
	if t.lastMemberTaskActivityAt.Valid {
		lastActivity = "last member-task activity was " + ago(now.Sub(t.lastMemberTaskActivityAt.Time)) + " ago"
	}
	running := fmt.Sprintf("%d member task(s) currently running on this issue/sub-issues", t.runningMemberTasks)

	if t.stalled(now) {
		return "Squad heartbeat telemetry for this issue:\n" +
			"- " + running + "\n" +
			"- " + lastActivity + "\n" +
			"Verdict: STALLED. No member is currently working and there is no recent member activity while the issue is still open. " +
			"You MUST re-dispatch the next step to a suitable member now — post an @mention delegation OR create a `todo` child issue assigned to a member — and record outcome `action`. " +
			"Do NOT record `no_action` and do NOT claim members are still working: the task queue above proves they are not. " +
			"If the work is genuinely complete, close the issue instead of recording no_action."
	}
	return "Squad heartbeat telemetry for this issue:\n" +
		"- " + running + "\n" +
		"- " + lastActivity + "\n" +
		"Verdict: PROGRESSING. The squad is genuinely active. Record outcome `no_action` and exit, " +
		"unless you spot a concrete next step that genuinely needs a new dispatch (in which case record `action`)."
}

// ago formats a duration as a short human-readable relative string for the
// handoff note (e.g. "3h12m", "47m", "25s"). Coarse is fine — it is telemetry
// for the leader, not an audit value.
func ago(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// squadHeartbeatStore is the narrow read contract the heartbeat inspector
// needs from the DB layer. *db.Queries satisfies it structurally. Defined here
// so the handler is unit-testable without a live Postgres, mirroring
// squadHealthStore in jobs_squad_health.go.
type squadHeartbeatStore interface {
	ListSquadHeartbeatDueIssues(ctx context.Context) ([]db.ListSquadHeartbeatDueIssuesRow, error)
	GetIssue(ctx context.Context, id pgtype.UUID) (db.Issue, error)
	GetSquadHeartbeatMemberActivity(ctx context.Context, arg db.GetSquadHeartbeatMemberActivityParams) (db.GetSquadHeartbeatMemberActivityRow, error)
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
		stalled := 0
		progressing := 0
		// Single "now" for the tick so all issues in this run share one
		// stall threshold baseline; avoids per-issue clock jitter.
		now := time.Now()
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

			// Build a data-driven handoff note. The verdict (STALLED vs
			// PROGRESSING) is computed here from the task queue, not left to
			// the leader LLM — this is the fix for false no_action on idle
			// squads. A telemetry failure degrades to the fallback note but
			// still wakes the leader.
			note := squadHeartbeatFallbackNote
			activity, terr := queries.GetSquadHeartbeatMemberActivity(ctx, db.GetSquadHeartbeatMemberActivityParams{
				IssueID:  row.IssueID,
				LeaderID: row.LeaderID,
			})
			if terr != nil {
				// Fallback (no telemetry) is counted as stalled-unknown so the
				// counters stay honest: we did NOT prove progress.
				stalled++
				slog.Warn("squad heartbeat: member-activity telemetry failed; using fallback note",
					"squad_id", util.UUIDToString(row.SquadID),
					"issue_id", util.UUIDToString(row.IssueID),
					"error", terr)
			} else {
				tel := squadHeartbeatTelemetry{
					runningMemberTasks:       activity.RunningMemberTasks,
					lastMemberTaskActivityAt: activity.LastMemberTaskActivityAt,
					heartbeatInterval:        time.Duration(row.HeartbeatIntervalMinutes) * time.Minute,
				}
				if tel.stalled(now) {
					stalled++
				} else {
					progressing++
				}
				note = heartbeatHandoffNote(tel, now)
			}

			// Pass an invalid triggering agent: the heartbeat is a scheduler
			// tick, not a member action, so the self-trigger guard inside
			// EnqueueSquadLeaderFollowUp must not reject this wake-up.
			ok, ferr := follower.EnqueueSquadLeaderFollowUp(ctx, issue, pgtype.UUID{}, note)
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
				"candidates":  len(rows),
				"woken":       woken,
				"stalled":     stalled,
				"progressing": progressing,
			},
		}, nil
	}
}
