---
name: multica-squads
description: "Use when creating, inspecting, updating, assigning, mentioning, or debugging Multica squads. Explains what squads are, squad/member fields, CLI commands, leader routing, issue assignment, comments, mentions, autopilot behavior, leader briefing, side effects, and product-gap handling."
user-invocable: false
allowed-tools: Bash(multica *)
---

# Multica Squads

## Quick start

If debugging why a squad did or did not run, inspect first:

```bash
multica issue get <issue-id> --output json
multica squad get <squad-id> --output json
multica squad member list <squad-id> --output json
multica issue comment list <issue-id> --recent 10 --output json
```

If the command shape is unclear, check help instead of guessing:

```bash
multica squad --help
multica squad member --help
multica issue update --help
multica issue comment add --help
```

Do not assign, comment, mention, update, delete, or record squad activity just
to test. These can mutate workspace state or trigger agent runs.

## Core model

A Multica squad is a workspace routing and coordination object.

A squad is not an agent. It does not run work by itself. Current behavior:
squad-routed work runs through the squad's `leader_id` agent.

Important consequences:

- assigning an issue to a squad routes to the leader;
- mentioning a squad routes to the leader;
- squad-assigned autopilot resolves to the leader;
- squad members are not automatically fanned out;
- squad `instructions` are leader briefing content, not member prompts.

## CLI

Squad commands:

```bash
multica squad list --output json
multica squad get <squad-id> --output json
multica squad create --name <name> --leader <agent-name-or-id> --output json
multica squad update <squad-id> --instructions "<leader coordination policy>" --output json
multica squad delete <squad-id>
```

Member commands:

```bash
multica squad member list <squad-id> --output json
multica squad member add <squad-id> --member-id <id> --type agent|member --role <role> --output json
multica squad member remove <squad-id> --member-id <id> --type agent|member
multica squad member set-role <squad-id> --member-id <id> --member-type agent|member --role <role> --output json
```

Squad leader evaluation command:

```bash
multica squad activity <issue-id> action|no_action|failed --reason "<why>" --output json
```

`activity` is a write: it records the leader's evaluation decision on an issue.
Use it only when acting as the squad leader after evaluating a trigger.

Write `--reason` in the SAME language as the issue's title and its recent
comments — no exceptions, for every outcome (`action`, `no_action`, `failed`).
Coordinative turns (no_action, summaries, approval requests) are NOT exempt;
they are the most common source of wrong-language reasons. The reason is shown
verbatim on the squad "Inspections" panel for the whole team, so an English
reason on a Chinese issue is visible breakage. Keep it to one or two concise
sentences.

Use `action` ONLY when you actually dispatched a member this turn (an
@mention delegation or a child issue assigned to a member). Coordinative
turns with no dispatch — summarising, confirming progress, requesting
approval — must be recorded as `no_action`. The backend verifies every
`action` claim against real member dispatch and marks unverified ones on the
Inspections panel.

Issue/comment commands often needed with squads:

```bash
multica issue get <issue-id> --output json
multica issue update <issue-id> --help
multica issue comment list <issue-id> --output json
multica issue comment add <issue-id> --help
```

Prefer `--output json` for reads. Use `--help` before writes.

## Squad fields

- `id` — squad UUID.
- `workspace_id` — workspace the squad belongs to.
- `name` — display name; unique per workspace.
- `description` — human-facing metadata/display text. Do not assume runtime
  prompt impact unless source proves a consumer.
- `instructions` — squad-level instructions added to the squad leader briefing.
  They are not directly injected into every squad member.
- `avatar_url` — optional squad avatar URL.
- `leader_id` — agent ID of the squad leader; the runtime target for
  squad-routed work.
- `creator_id` — creator of the squad.
- `archived_at` / `archived_by` — archive metadata. Archived squads are rejected
  by assignment/autopilot routing paths.
- `member_count` — list response count of squad members.
- `member_preview` — list response preview of squad members.

Use `instructions` for leader-facing coordination policy: squad responsibility,
delegation expectations, when to ask humans, and review/handoff rules. Do not
write it as if every member automatically receives it.

## Squad member fields

- `member_type` — `agent` or `member`.
- `member_id` — ID of the agent or workspace member.
- `role` — roster role label. Current behavior: non-empty `role` appears in the
  leader briefing roster. Do not assume it creates scheduling, permissions, or
  routing behavior.

## Creation and leader membership

Creating a squad requires `leader_id`. The leader must be a workspace agent.
Create/update does not reject an archived leader: the lookup only checks the
agent exists in the workspace. An archived leader fails closed later, at
routing/dispatch — assignment, autopilot admission, and the comment/mention
readiness gate all reject an archived leader before any task is enqueued.

On create, the backend attempts to add the leader as a squad member with role
`leader`. When updating `leader_id`, if the new leader is not already a member,
the backend adds the new leader as a squad member with role `leader`.

## Leader briefing

For squad leader tasks, Multica appends a squad leader briefing to the leader
agent instructions. The briefing includes:

- Squad Operating Protocol;
- Squad Roster;
- Squad Instructions, only when `instructions` is non-empty.

Roster entries include member name, member type, mention markdown, and non-empty
role. For agent members the roster also lists their assigned skills
(`skills: a, b`, or `no skills assigned` when the agent has none) so the leader
can delegate by capability instead of guessing from the role label; human
members carry no skills segment. Builtin `multica-*` skills are not listed —
only the workspace skills explicitly attached to the agent. Archived agent
members are skipped from the briefing roster.

## Issue assignment behavior

Issues can be assigned to squads with:

```text
assignee_type = "squad"
assignee_id = <squad-id>
```

Current behavior:

- assignment routes work to `squad.leader_id`;
- it does not enqueue every squad member;
- assignment while status is `backlog` does not immediately start work;
- moving a squad-assigned issue out of `backlog` can trigger the leader;
- changing assignee cancels existing tasks for the issue before enqueueing the
  new assignee path.

Assignment validation rejects a missing type/id pair, non-existent squad,
archived squad, archived leader, and private leader when the actor cannot access
it.

## Comment and mention behavior

If an issue is assigned to a squad, a new comment can wake the squad leader. This
is leader routing, not member fan-out.

Squad mention format:

```md
[@Squad Name](mention://squad/<squad-id>)
```

Current behavior: resolve the squad, read `leader_id`, enqueue a leader task,
and use the current comment as the trigger comment. It does not enqueue every
squad member.

## Periodic health and heartbeat behavior

A squad leader is also woken automatically, without any human action, by two
background schedulers:

- **Stall detection** (`squad_health_inspect`): runs every 15 minutes. A
  squad-assigned issue is "stalled" when a non-leader member has terminal work
  (failed/cancelled/completed), no non-leader member is currently active on
  it, and the leader has no queued/dispatched task. Each stalled issue enqueues
  one leader follow-up.
- **Periodic heartbeat** (`squad_heartbeat_inspect`): runs on a fixed base tick
  and wakes the leader for any open (non-`done`/`cancelled`) squad-assigned
  issue whose last `squad_leader_evaluated` activity is older than the squad's
  configured `heartbeat_interval_minutes` (default 30, configurable on the
  squad Inspections panel, range 5–1440 minutes), as long as the leader has no
  queued/dispatched task. The leader verifies actual member progress: if a
  member is currently working, it records `no_action`; if no member is working
  and the issue is still open (members idle), it treats that as a stall and
  re-dispatches the next step (`action`). The heartbeat is how a squad breaks
  out of a silent stall where members finished or went idle without the issue
  moving forward.

Both paths enqueue via the same deduped leader-follow-up entry point, so a
race between them cannot produce a duplicate wake-up.

## Autopilot behavior

Autopilots can be assigned to squads. For `assignee_type = "squad"`:

- executable agent resolves from `squad.leader_id`;
- admission/readiness checks run against the leader;
- archived squads fail closed / skip dispatch;
- run attribution records squad id where applicable.

For `create_issue` autopilots, the created issue keeps `assignee_type = "squad"`
and `assignee_id = <squad-id>`, while the actual executing agent is the resolved
leader. For `run_only` autopilots, no issue is created; the task is created
directly for the resolved leader agent.

## Handling complaints or product gaps

When the user says squad behavior is wrong, confusing, or disappointing, do not
immediately assume code is broken and do not defend current behavior just because
it exists. Classify first:

- expected current behavior;
- configuration issue;
- product limitation;
- actual bug.

Explain the current source-backed behavior. If the behavior is technically
correct but product-wise bad, say so and propose a scoped product/code change.

Do not silently change squad routing, member fan-out, leader briefing, autopilot
behavior, or comment-trigger behavior without confirmation. These are product
contract changes with side effects.

## Side effects

These actions can trigger agent work or mutate durable state:

- creating a squad;
- updating squad fields;
- changing `leader_id`;
- adding/removing members;
- changing member roles;
- assigning an issue to a squad;
- moving a squad-assigned issue out of backlog;
- commenting on a squad-assigned issue;
- mentioning a squad;
- creating or triggering squad-assigned autopilots;
- recording squad activity with `multica squad activity`;
- deleting/archive squad.

Do not perform side-effecting actions as tests unless the user explicitly
authorizes them.

## Common wrong assumptions

- A squad is not an agent.
- Squad work routes to `leader_id`, not every member.
- Squad mention routes to the leader, not every member.
- Squad assignment routes to the leader, not every member.
- Squad autopilot resolves to the leader as executable agent.
- `instructions` are leader briefing content, not automatic member prompts.
- `description` is not proven runtime prompt content.
- `role` is roster context, not automatic scheduling.
- Backlog assignment does not immediately start work.

## References

For source paths, tests, edge cases, and exact routing details, see:

```text
references/squad-source-map.md
```
