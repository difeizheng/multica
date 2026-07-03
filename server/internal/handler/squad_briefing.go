package handler

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// squadOperatingProtocol is the hard-coded system-level briefing prepended to
// every squad-leader claim. It explains the leader's coordinator role, the
// @mention dispatch mechanism, and the stop-after-dispatch contract.
//
// Keep this text English-only (matches existing agent-harness conventions)
// and keep the mention syntax exactly aligned with util.MentionRe — the
// "Squad Roster" block below renders concrete examples that round-trip
// through util.ParseMentions, and the protocol text refers to that format.
const squadOperatingProtocol = `## Squad Operating Protocol

**If you are reading this section, you have been activated as a squad LEADER
for this task — regardless of how the work reached you (direct assignment,
an @squad mention in a comment, quick-create, or autopilot).** Your job is to
**coordinate**, NOT to do the work yourself. Even if the task reads like a
direct request to "do X" (review this PR, fix this bug, write this code), you
must delegate X to the right squad member by @mention — doing it yourself
defeats the entire purpose of the squad and is a protocol violation.

Your responsibilities, in order:

1. **Read the issue** (title, description, latest comments, acceptance
   criteria) and decide which squad member is best suited to do the work.
   Match the task to each member's listed **skills** and role in the Squad
   Roster below — prefer the member whose skills cover the work.
2. **Delegate by @mention.** Post a single comment on this issue that
   @mentions the chosen member(s) and tells them what to do.
   - **Be terse.** Every Multica agent already has full context of the
     issue (title, description, all prior comments, attachments) and
     the surrounding workspace. Do NOT restate or summarise the
     issue body, prior discussion, or known facts in your delegation
     comment — they read it themselves.
   - Say only what cannot be inferred from the issue: who you're
     picking, why them (one short clause), and any *additional*
     constraints, hints, or sequencing you want them to follow.
     Two or three sentences is usually plenty.
   - Use the exact mention markdown shown in the Squad Roster below —
     typing a plain "@name" will not trigger anyone.
3. **Record your evaluation.** After every trigger — whether you delegated,
   decided no action is needed, or encountered an error — record it:
   ` + "`" + `multica squad activity <issue-id> <outcome> --reason "<short reason>"` + "`" + `
   Outcome values: ` + "`" + `action` + "`" + ` (you delegated or acted),
   ` + "`" + `no_action` + "`" + ` (you evaluated and decided nothing is needed),
   ` + "`" + `failed` + "`" + ` (you hit an error).
   This is mandatory on every turn — it records your decision in the
   issue timeline so humans can see you evaluated the trigger.
   Write ` + "`" + `--reason` + "`" + ` in the SAME language as the issue's title and its
   recent comments — this is non-negotiable (see Hard rules below). The reason
   is shown verbatim on the squad "Inspections" panel for the whole team, so
   a reason in the wrong language reads as a bug to the team. If the issue is
   in Chinese, the reason MUST be Chinese; if English, English; and so on.
   Use ` + "`" + `action` + "`" + ` ONLY when you actually dispatched a member (an
   @mention delegation or a child issue assigned to a member). Coordinative
   turns with no dispatch — summarising, confirming progress, requesting
   approval — must be recorded as ` + "`" + `no_action` + "`" + `. The backend
   verifies ` + "`" + `action` + "`" + ` claims against real member dispatch and flags
   unverified ones on the Inspections panel.
4. **Stop after dispatching.** Once your delegation comment is posted
   and evaluation recorded, end your turn. Do not continue working,
   do not write code, do not open files. You will be re-triggered
   automatically when:
   - a delegated member posts an update or asks you a question;
   - a delegated member finishes and the issue moves forward;
   - someone @mentions you again on this issue.
5. **Re-evaluate on each trigger.** When you wake up again, read the new
   activity and decide whether to delegate the next step, escalate to
   the human reporter, or close the loop. If no action is needed
   (e.g. a member posted a progress update that requires no response),
   record ` + "`" + `no_action` + "`" + ` and exit silently.
6. **Stalled-issue follow-ups.** You may also be woken automatically by a
   squad health check or because a member's task just failed or was
   cancelled — the handoff note in your opening context will say so. This
   means the issue has stalled: a member stopped (failed, cancelled, or
   finished a slice) and nobody is currently active on it. Read the issue's
   recent activity to see what the member attempted and why they stopped,
   then either re-dispatch to the same member or a better-suited one,
   escalate to the human reporter, or record ` + "`" + `no_action` + "`" + ` if
   the work is genuinely complete. Do not assume a single failure means
   the member is incapable — transient runtime errors are common; re-dispatch
   once before escalating.
7. **Periodic heartbeat check-ins.** You may also be woken automatically by
   a periodic heartbeat — a routine check-in that fires on a fixed cadence
   (every N minutes, configured per squad). The handoff note will say it is a
   heartbeat. A heartbeat does NOT mean "everything is fine" — you must
   verify the squad's actual progress yourself:
   - Look at when each member LAST produced work (a completed task, a
     comment, a status change, a code change). Do not infer "members are
     busy" from the issue being ` + "`" + `in_progress` + "`" + ` alone — ` + "`" + `in_progress` + "`" + ` is
     a label, not evidence of activity.
   - If a member is currently running a task on this issue, or produced
     work within the last working step, the squad is genuinely progressing:
     record ` + "`" + `no_action` + "`" + ` and exit.
   - If NO member is currently working AND no member has produced anything
     recently (e.g. the last member activity is many hours or days old, or
     the last member task completed long ago) while the issue is still
     open, the work has effectively STALLED even though nothing reported an
     error. In that case the heartbeat IS the signal that progress stopped:
     re-dispatch the next step to the right member now (that is an
     ` + "`" + `action` + "`" + `, NOT a ` + "`" + `no_action` + "`" + `), exactly as you would for a stall.
   - Never record ` + "`" + `no_action` + "`" + ` for an open issue whose members are all idle.
     Recording ` + "`" + `no_action` + "`" + ` when nothing is actually happening is how squads
     silently stall — the heartbeat is your chance to break the stall.

Hard rules:
- The ` + "`" + `--reason` + "`" + ` text MUST be written in the SAME language as the
  issue's title and its recent comments — no exceptions, and no mixing.
  This applies to EVERY outcome: ` + "`" + `action` + "`" + `, ` + "`" + `no_action` + "`" + `, and
  ` + "`" + `failed` + "`" + `. Coordinative turns (no_action, summary, approval requests) are
  NOT exempt — they are the most common source of wrong-language reasons.
  If the issue is in 简体中文, write the reason in 简体中文
  (e.g. ` + "`" + `--reason "成员正在执行前端任务，本轮无需追加派单"` + "`" + `),
  never in English. The reason is shown verbatim to the whole team on the
  Inspections panel, so an English reason on a Chinese issue is visible
  breakage.
- EVERY delegation MUST use the full mention markdown syntax
  ` + "`" + `[@Name](mention://<type>/<UUID>)` + "`" + ` exactly as shown in the Squad
  Roster. A plain "@name" or bare name does NOT trigger the agent —
  if you skip the mention link, the task is never delivered and the
  issue stalls. This is non-negotiable: no mention link = no delegation.
- Do NOT restate the issue body or prior comments in your delegation —
  the assignee already has them. Repeating context is noise that
  buries the actual instruction.
- Do NOT do the implementation work yourself unless the squad has no
  other suitable members. The squad exists so work is split — bypassing
  it defeats the point.
- Do NOT @mention members who don't appear in the Squad Roster below;
  they are not part of this squad.
- One delegation comment per turn is enough. Avoid spamming multiple
  near-identical comments.
- If the squad has no member capable of the task, post a comment
  explaining the gap (and @mention the issue's reporter if possible)
  rather than silently doing the work.
- ALWAYS call ` + "`" + `multica squad activity` + "`" + ` before ending your turn —
  even when the outcome is no_action.
- A child issue you create with ` + "`" + `--status todo` + "`" + ` and an agent assignee
  already fires that agent automatically — the assignment IS the trigger.
  If you also @mention the same agent on this parent issue for the same
  work, the agent runs twice in parallel (once from the mention, once
  from the assignment). Pick exactly one path: either delegate by
  @mention on this issue, or create a ` + "`" + `todo` + "`" + ` child issue assigned to
  them. Never both for the same work.`

// buildSquadLeaderBriefing composes the full system briefing appended to a
// squad leader's Instructions when it claims a task on a squad-assigned
// issue. The returned string contains three sections:
//
//  1. Squad Operating Protocol (constant, system-level rules).
//  2. Squad Roster (data — leader self-row + members with literal
//     `[@Name](mention://<type>/<UUID>)` strings ready to paste).
//  3. Squad Instructions (user-defined `squad.instructions`, omitted when
//     empty so we don't leave a dangling heading).
//
// Archived agent members are skipped — there's no point asking the leader
// to delegate to a retired agent. Members whose underlying record can't be
// loaded (deleted user/agent races, FK weirdness) are also skipped silently.
func buildSquadLeaderBriefing(ctx context.Context, q *db.Queries, squad db.Squad) string {
	var sb strings.Builder
	sb.WriteString(squadOperatingProtocol)
	sb.WriteString("\n\n")
	sb.WriteString(buildSquadRoster(ctx, q, squad))

	if trimmed := strings.TrimSpace(squad.Instructions); trimmed != "" {
		sb.WriteString("\n\n## Squad Instructions (")
		sb.WriteString(squad.Name)
		sb.WriteString(")\n\n")
		sb.WriteString(trimmed)
	}
	return sb.String()
}

// buildSquadRoster renders the "## Squad Roster" section: a leader self-row
// plus one row per non-archived member, with literal mention markdown.
func buildSquadRoster(ctx context.Context, q *db.Queries, squad db.Squad) string {
	var sb strings.Builder
	sb.WriteString("## Squad Roster\n\n")

	// Leader self-row. Leaders are always agents (FK enforced in schema).
	leaderName := "Leader"
	if leader, err := q.GetAgent(ctx, squad.LeaderID); err == nil {
		leaderName = leader.Name
	}
	sb.WriteString("Leader (you):\n")
	sb.WriteString("- ")
	sb.WriteString(leaderName)
	sb.WriteString(" — agent — `")
	sb.WriteString(formatMention(leaderName, "agent", util.UUIDToString(squad.LeaderID)))
	sb.WriteString("`\n")

	members, err := q.ListSquadMembers(ctx, squad.ID)
	if err != nil {
		members = nil
	}

	skillNamesByAgentID, skillsLoaded := loadSquadMemberSkillNames(ctx, q, members, util.UUIDToString(squad.LeaderID))

	rows := make([]string, 0, len(members))
	for _, m := range members {
		// Skip the leader if they happen to also be in the member list —
		// they're already shown above and we don't want self-delegation.
		if m.MemberType == "agent" && util.UUIDToString(m.MemberID) == util.UUIDToString(squad.LeaderID) {
			continue
		}
		row := renderMemberRow(ctx, q, m, skillNamesByAgentID, skillsLoaded)
		if row != "" {
			rows = append(rows, row)
		}
	}

	if len(rows) == 0 {
		sb.WriteString("\nMembers: (none — you are the only member of this squad)\n")
		return sb.String()
	}

	sb.WriteString("\nMembers:\n")
	for _, r := range rows {
		sb.WriteString(r)
	}
	return sb.String()
}

func loadSquadMemberSkillNames(ctx context.Context, q *db.Queries, members []db.SquadMember, leaderID string) (map[string][]string, bool) {
	agentIDs := make([]pgtype.UUID, 0)
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		if m.MemberType != "agent" {
			continue
		}
		id := util.UUIDToString(m.MemberID)
		if id == leaderID {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		agentIDs = append(agentIDs, m.MemberID)
	}
	if len(agentIDs) == 0 {
		return map[string][]string{}, true
	}
	rows, err := q.ListAgentSkillNamesByAgentIDs(ctx, agentIDs)
	if err != nil {
		return nil, false
	}
	byAgentID := make(map[string][]string, len(agentIDs))
	for _, row := range rows {
		id := util.UUIDToString(row.AgentID)
		byAgentID[id] = append(byAgentID[id], row.Name)
	}
	return byAgentID, true
}

// renderMemberRow renders a single roster row, returning "" if the member
// can't be resolved or should be skipped (e.g. archived agent).
func renderMemberRow(ctx context.Context, q *db.Queries, m db.SquadMember, skillNamesByAgentID map[string][]string, skillsLoaded bool) string {
	id := util.UUIDToString(m.MemberID)
	role := strings.TrimSpace(m.Role)
	switch m.MemberType {
	case "agent":
		ag, err := q.GetAgent(ctx, m.MemberID)
		if err != nil {
			return ""
		}
		if ag.ArchivedAt.Valid {
			return ""
		}
		// Agents carry skills; surfacing them lets the leader delegate by
		// capability instead of guessing from the free-text role label.
		return formatRosterRow(ag.Name, "agent", role, agentSkillsRosterSegment(skillNamesByAgentID, skillsLoaded, id), formatMention(ag.Name, "agent", id))
	case "member":
		user, err := q.GetUser(ctx, m.MemberID)
		if err != nil {
			return ""
		}
		// Mention syntax for humans uses the user_id (matches the rest of
		// the product — see util.MentionRe and frontend mention payloads).
		// Humans have no Multica skills, so no skills segment is rendered.
		userID := util.UUIDToString(m.MemberID)
		return formatRosterRow(user.Name, "member (human)", role, "", formatMention(user.Name, "member", userID))
	default:
		return ""
	}
}

// agentSkillsRosterSegment returns the roster segment describing an agent
// member's assigned skills. "skills: a, b" when the agent has skills (the
// names are pre-sorted by ListAgentSkillNamesByAgentIDs), "no skills assigned"
// when it has none so the leader knows the capability is genuinely absent, and
// "" only when the lookup fails — a transient DB error degrades to the prior
// name+role row rather than asserting a misleading "no skills".
func agentSkillsRosterSegment(skillNamesByAgentID map[string][]string, skillsLoaded bool, agentID string) string {
	if !skillsLoaded {
		return ""
	}
	names := skillNamesByAgentID[agentID]
	if len(names) == 0 {
		return "no skills assigned"
	}
	return "skills: " + strings.Join(names, ", ")
}

func formatRosterRow(name, kind, role, skills, mention string) string {
	var sb strings.Builder
	sb.WriteString("- ")
	sb.WriteString(name)
	sb.WriteString(" — ")
	sb.WriteString(kind)
	if role != "" {
		sb.WriteString(`, role: "`)
		sb.WriteString(role)
		sb.WriteString(`"`)
	}
	if skills != "" {
		sb.WriteString(" — ")
		sb.WriteString(skills)
	}
	sb.WriteString(" — `")
	sb.WriteString(mention)
	sb.WriteString("`\n")
	return sb.String()
}

// formatMention emits a mention markdown string that round-trips through
// util.ParseMentions. The label is the human display name; the link target
// uses the mention:// scheme with the entity type and UUID.
func formatMention(name, mentionType, id string) string {
	return "[@" + name + "](mention://" + mentionType + "/" + id + ")"
}
