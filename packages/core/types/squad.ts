export type SquadMemberType = "agent" | "member";

export type SquadActivityOutcome = "action" | "no_action" | "failed";

export interface SquadMemberPreview {
  member_type: SquadMemberType;
  member_id: string;
  role: string;
}

export interface Squad {
  id: string;
  workspace_id: string;
  name: string;
  description: string;
  instructions: string;
  heartbeat_interval_minutes: number;
  avatar_url: string | null;
  leader_id: string;
  creator_id: string;
  created_at: string;
  updated_at: string;
  archived_at: string | null;
  archived_by: string | null;
  member_count?: number;
  member_preview?: SquadMemberPreview[];
}

export interface SquadMember {
  id: string;
  squad_id: string;
  member_type: SquadMemberType;
  member_id: string;
  role: string;
  created_at: string;
}

export interface SquadActivityLog {
  id: string;
  squad_id: string;
  issue_id: string;
  trigger_comment_id: string | null;
  leader_id: string;
  outcome: SquadActivityOutcome;
  details: unknown;
  created_at: string;
}

export interface CreateSquadRequest {
  name: string;
  description?: string;
  leader_id: string;
  avatar_url?: string;
}

export interface UpdateSquadRequest {
  name?: string;
  description?: string;
  instructions?: string;
  leader_id?: string;
  avatar_url?: string;
  heartbeat_interval_minutes?: number;
}

export interface AddSquadMemberRequest {
  member_type: SquadMemberType;
  member_id: string;
  role?: string;
}

export interface RemoveSquadMemberRequest {
  member_type: SquadMemberType;
  member_id: string;
}

export interface UpdateSquadMemberRoleRequest {
  member_type: SquadMemberType;
  member_id: string;
  role: string;
}

export interface CreateSquadActivityLogRequest {
  squad_id: string;
  issue_id: string;
  trigger_comment_id?: string;
  outcome: SquadActivityOutcome;
  details?: unknown;
}

// SquadMemberStatus mirrors the five-way bucket the back-end derives in
// handler/squad.go::deriveSquadMemberStatus. Kept as a string union here
// (rather than re-derived from snapshot data) so the squad page can render
// the freshest server-side judgement without re-fetching the agent
// snapshot / runtime list. `archived` wins over every runtime/task signal.
export type SquadMemberStatusValue =
  | "working"
  | "idle"
  | "offline"
  | "unstable"
  | "archived";

export interface SquadActiveIssueBrief {
  issue_id: string;
  identifier: string;
  title: string;
  issue_status: string;
}

export interface SquadMemberStatus {
  member_type: SquadMemberType;
  member_id: string;
  // Human members are returned with status === null so the UI can render
  // them in the same list without showing a status pill (v1 has no
  // presence signal for humans).
  status: SquadMemberStatusValue | null;
  active_issues: SquadActiveIssueBrief[];
  last_active_at: string | null;
}

export interface SquadMemberStatusListResponse {
  members: SquadMemberStatus[];
}

// SquadInspectionRecord is one leader wake-up + evaluation, sourced from the
// `squad_leader_evaluated` activity the leader records each time it is woken
// (by the periodic inspector, the terminal-state hook, an @mention, or an
// assign). Drives the read-only "Inspections" tab on the squad detail page.
export interface SquadInspectionRecord {
  id: string;
  issue_id: string;
  issue_number: number | null;
  issue_title: string;
  issue_status: string;
  outcome: SquadActivityOutcome | string;
  reason: string;
  created_at: string;
  duration_ms: number | null;
  // Only present on outcome=action rows written after dispatch verification
  // shipped. null on legacy rows and on no_action/failed outcomes — treat as
  // verified. `false` means the leader claimed `action` but no member task was
  // spawned that turn (e.g. it only summarised or requested approval).
  verified: boolean | null;
}

export interface SquadInspectionHistoryResponse {
  entries: SquadInspectionRecord[];
}
