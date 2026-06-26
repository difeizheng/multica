package service

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func uuidFromBytes(s string) pgtype.UUID {
	u := pgtype.UUID{Valid: true}
	copy(u.Bytes[:], []byte(s))
	return u
}

func TestShouldEnqueueSquadLeaderFollowUp(t *testing.T) {
	leader := uuidFromBytes("leader-leader-leader-leader01")
	member := uuidFromBytes("member-member-member-member01")

	activeSquad := db.Squad{LeaderID: leader}
	archivedSquad := db.Squad{LeaderID: leader, ArchivedAt: pgtype.Timestamptz{Valid: true}}
	noLeaderSquad := db.Squad{LeaderID: pgtype.UUID{}}

	issue := db.Issue{ID: uuidFromBytes("issue-issue-issue-issue-0001")}

	tests := []struct {
		name            string
		issue           db.Issue
		squad           db.Squad
		triggeringAgent pgtype.UUID
		leaderPending   bool
		want            bool
	}{
		{
			name:            "active squad, member trigger, no pending -> enqueue",
			issue:           issue,
			squad:           activeSquad,
			triggeringAgent: member,
			leaderPending:   false,
			want:            true,
		},
		{
			name:            "archived squad -> skip",
			issue:           issue,
			squad:           archivedSquad,
			triggeringAgent: member,
			leaderPending:   false,
			want:            false,
		},
		{
			name:            "squad without leader -> skip",
			issue:           issue,
			squad:           noLeaderSquad,
			triggeringAgent: member,
			leaderPending:   false,
			want:            false,
		},
		{
			name:            "self-trigger (member is the leader) -> skip",
			issue:           issue,
			squad:           activeSquad,
			triggeringAgent: leader,
			leaderPending:   false,
			want:            false,
		},
		{
			name:            "leader already has pending task -> skip (dedup)",
			issue:           issue,
			squad:           activeSquad,
			triggeringAgent: member,
			leaderPending:   true,
			want:            false,
		},
		{
			name:            "invalid triggering agent (scheduler path) still enqueues",
			issue:           issue,
			squad:           activeSquad,
			triggeringAgent: pgtype.UUID{},
			leaderPending:   false,
			want:            true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldEnqueueSquadLeaderFollowUp(tc.issue, tc.squad, tc.triggeringAgent, tc.leaderPending)
			if got != tc.want {
				t.Fatalf("shouldEnqueueSquadLeaderFollowUp = %v, want %v", got, tc.want)
			}
		})
	}
}
