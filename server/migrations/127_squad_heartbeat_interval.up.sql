-- Per-squad periodic heartbeat inspection interval, in minutes. Drives the
-- squad_heartbeat_inspect scheduler job: a squad-assigned issue is re-inspected
-- when its last squad_leader_evaluated activity is older than this interval.
-- Default 30 minutes; UI clamps to [5, 1440]. NOT NULL so every squad always
-- has a concrete cadence (existing rows backfill to the default).
ALTER TABLE squad ADD COLUMN heartbeat_interval_minutes INTEGER NOT NULL DEFAULT 30;
