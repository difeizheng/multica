package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeSquadHeartbeatStore stubs squadHeartbeatStore for handler tests. GetIssue
// mirrors fakeSquadHealthStore so the same "issue disappeared between scan and
// load" race can be exercised. GetSquadHeartbeatMemberActivity returns either
// the configured telemetry for an issue, a per-issue error, or a zero row
// (running=0, no activity → stalled) when unconfigured.
type fakeSquadHeartbeatStore struct {
	candidates  []db.ListSquadHeartbeatDueIssuesRow
	issues      map[pgtype.UUID]db.Issue
	telemetry   map[pgtype.UUID]db.GetSquadHeartbeatMemberActivityRow
	activityErr map[pgtype.UUID]error
	issueErr    map[pgtype.UUID]error
	listErr     error
}

func (f *fakeSquadHeartbeatStore) ListSquadHeartbeatDueIssues(ctx context.Context) ([]db.ListSquadHeartbeatDueIssuesRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.candidates, nil
}

func (f *fakeSquadHeartbeatStore) GetIssue(ctx context.Context, id pgtype.UUID) (db.Issue, error) {
	if f.issueErr != nil {
		if err, ok := f.issueErr[id]; ok {
			return db.Issue{}, err
		}
	}
	issue, ok := f.issues[id]
	if !ok {
		return db.Issue{}, pgx.ErrNoRows
	}
	return issue, nil
}

func (f *fakeSquadHeartbeatStore) GetSquadHeartbeatMemberActivity(ctx context.Context, arg db.GetSquadHeartbeatMemberActivityParams) (db.GetSquadHeartbeatMemberActivityRow, error) {
	if f.activityErr != nil {
		if err, ok := f.activityErr[arg.IssueID]; ok {
			return db.GetSquadHeartbeatMemberActivityRow{}, err
		}
	}
	if f.telemetry != nil {
		if row, ok := f.telemetry[arg.IssueID]; ok {
			return row, nil
		}
	}
	// Default: zero row → no running tasks, no activity ever → stalled.
	return db.GetSquadHeartbeatMemberActivityRow{}, nil
}

func squadHeartbeatRow(id string) db.ListSquadHeartbeatDueIssuesRow {
	return db.ListSquadHeartbeatDueIssuesRow{
		SquadID:                  pgtype.UUID{Bytes: byteID(id + "_squad"), Valid: true},
		LeaderID:                 pgtype.UUID{Bytes: byteID(id + "_leader"), Valid: true},
		IssueID:                  pgtype.UUID{Bytes: byteID(id + "_issue"), Valid: true},
		WorkspaceID:              pgtype.UUID{Bytes: byteID(id + "_ws"), Valid: true},
		HeartbeatIntervalMinutes: 30,
	}
}

func TestSquadHeartbeatInspectHandler(t *testing.T) {
	t.Run("empty due list wakes no one", func(t *testing.T) {
		store := &fakeSquadHeartbeatStore{candidates: nil, issues: map[pgtype.UUID]db.Issue{}}
		follower := &fakeFollower{}
		handler := makeSquadHeartbeatInspectHandler(store, follower)

		res, err := handler(context.Background(), HandlerInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RowsAffected != 0 {
			t.Fatalf("RowsAffected = %d, want 0", res.RowsAffected)
		}
		if len(follower.enqueued) != 0 {
			t.Fatalf("expected no enqueues, got %d", len(follower.enqueued))
		}
	})

	t.Run("wakes one leader per due issue", func(t *testing.T) {
		r1 := squadHeartbeatRow("aaa")
		r2 := squadHeartbeatRow("bbb")
		store := &fakeSquadHeartbeatStore{
			candidates: []db.ListSquadHeartbeatDueIssuesRow{r1, r2},
			issues: map[pgtype.UUID]db.Issue{
				r1.IssueID: {ID: r1.IssueID},
				r2.IssueID: {ID: r2.IssueID},
			},
		}
		follower := &fakeFollower{}
		handler := makeSquadHeartbeatInspectHandler(store, follower)

		res, err := handler(context.Background(), HandlerInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RowsAffected != 2 {
			t.Fatalf("RowsAffected = %d, want 2", res.RowsAffected)
		}
		if len(follower.enqueued) != 2 {
			t.Fatalf("expected 2 enqueues, got %d", len(follower.enqueued))
		}
		if got := res.Result["candidates"]; got != 2 {
			t.Fatalf("candidates = %v, want 2", got)
		}
		if got := res.Result["woken"]; got != 2 {
			t.Fatalf("woken = %v, want 2", got)
		}
	})

	t.Run("missing issue (closed between scan and load) is skipped, not fatal", func(t *testing.T) {
		r1 := squadHeartbeatRow("aaa")
		r2 := squadHeartbeatRow("bbb")
		store := &fakeSquadHeartbeatStore{
			candidates: []db.ListSquadHeartbeatDueIssuesRow{r1, r2},
			issues: map[pgtype.UUID]db.Issue{
				r1.IssueID: {ID: r1.IssueID},
			},
		}
		follower := &fakeFollower{}
		handler := makeSquadHeartbeatInspectHandler(store, follower)

		res, err := handler(context.Background(), HandlerInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RowsAffected != 1 {
			t.Fatalf("RowsAffected = %d, want 1 (only r1)", res.RowsAffected)
		}
		if len(follower.enqueued) != 1 || follower.enqueued[0] != r1.IssueID {
			t.Fatalf("expected only r1 enqueued, got %v", follower.enqueued)
		}
	})

	t.Run("follower error for one issue does not abort the tick", func(t *testing.T) {
		r1 := squadHeartbeatRow("aaa")
		r2 := squadHeartbeatRow("bbb")
		store := &fakeSquadHeartbeatStore{
			candidates: []db.ListSquadHeartbeatDueIssuesRow{r1, r2},
			issues: map[pgtype.UUID]db.Issue{
				r1.IssueID: {ID: r1.IssueID},
				r2.IssueID: {ID: r2.IssueID},
			},
		}
		follower := &fakeFollower{
			failOn: map[pgtype.UUID]error{r1.IssueID: errors.New("boom")},
		}
		handler := makeSquadHeartbeatInspectHandler(store, follower)

		res, err := handler(context.Background(), HandlerInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RowsAffected != 1 {
			t.Fatalf("RowsAffected = %d, want 1 (r1 failed, r2 ok)", res.RowsAffected)
		}
		if len(follower.enqueued) != 1 || follower.enqueued[0] != r2.IssueID {
			t.Fatalf("expected only r2 enqueued, got %v", follower.enqueued)
		}
	})

	t.Run("list error propagates", func(t *testing.T) {
		store := &fakeSquadHeartbeatStore{listErr: errors.New("db down")}
		follower := &fakeFollower{}
		handler := makeSquadHeartbeatInspectHandler(store, follower)

		if _, err := handler(context.Background(), HandlerInput{}); err == nil {
			t.Fatalf("expected error from list failure")
		}
	})

	// Verdict-driven cases: the handoff note must carry a STALLED verdict and
	// push `action` when no member is working, and a PROGRESSING verdict with
	// `no_action` when a member task is running or recently active. These
	// encode the core fix — the leader no longer guesses from in_progress.

	t.Run("stalled issue gets STALLED verdict pushing action", func(t *testing.T) {
		r := squadHeartbeatRow("aaa")
		store := &fakeSquadHeartbeatStore{
			candidates: []db.ListSquadHeartbeatDueIssuesRow{r},
			issues:     map[pgtype.UUID]db.Issue{r.IssueID: {ID: r.IssueID}},
			// telemetry left unset → zero row → stalled
		}
		follower := &fakeFollower{}
		handler := makeSquadHeartbeatInspectHandler(store, follower)

		res, err := handler(context.Background(), HandlerInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(follower.reasons) != 1 {
			t.Fatalf("expected 1 reason, got %d", len(follower.reasons))
		}
		note := follower.reasons[0]
		if !strings.Contains(note, "Verdict: STALLED") {
			t.Fatalf("expected STALLED verdict in note, got: %s", note)
		}
		if !strings.Contains(note, "`action`") && !strings.Contains(note, "action") {
			t.Fatalf("expected note to push action, got: %s", note)
		}
		if got := res.Result["stalled"]; got != 1 {
			t.Fatalf("stalled = %v, want 1", got)
		}
		if got := res.Result["progressing"]; got != 0 {
			t.Fatalf("progressing = %v, want 0", got)
		}
	})

	t.Run("running member task yields PROGRESSING verdict", func(t *testing.T) {
		r := squadHeartbeatRow("aaa")
		store := &fakeSquadHeartbeatStore{
			candidates: []db.ListSquadHeartbeatDueIssuesRow{r},
			issues:     map[pgtype.UUID]db.Issue{r.IssueID: {ID: r.IssueID}},
			telemetry: map[pgtype.UUID]db.GetSquadHeartbeatMemberActivityRow{
				r.IssueID: {RunningMemberTasks: 2},
			},
		}
		follower := &fakeFollower{}
		handler := makeSquadHeartbeatInspectHandler(store, follower)

		res, err := handler(context.Background(), HandlerInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		note := follower.reasons[0]
		if !strings.Contains(note, "Verdict: PROGRESSING") {
			t.Fatalf("expected PROGRESSING verdict in note, got: %s", note)
		}
		if !strings.Contains(note, "no_action") {
			t.Fatalf("expected note to allow no_action, got: %s", note)
		}
		if got := res.Result["progressing"]; got != 1 {
			t.Fatalf("progressing = %v, want 1", got)
		}
		if got := res.Result["stalled"]; got != 0 {
			t.Fatalf("stalled = %v, want 0", got)
		}
	})

	t.Run("recent member activity within interval is progressing", func(t *testing.T) {
		r := squadHeartbeatRow("aaa") // 30-minute interval
		recent := pgtype.Timestamptz{Time: time.Now().Add(-5 * time.Minute), Valid: true}
		store := &fakeSquadHeartbeatStore{
			candidates: []db.ListSquadHeartbeatDueIssuesRow{r},
			issues:     map[pgtype.UUID]db.Issue{r.IssueID: {ID: r.IssueID}},
			telemetry: map[pgtype.UUID]db.GetSquadHeartbeatMemberActivityRow{
				r.IssueID: {RunningMemberTasks: 0, LastMemberTaskActivityAt: recent},
			},
		}
		follower := &fakeFollower{}
		handler := makeSquadHeartbeatInspectHandler(store, follower)

		res, err := handler(context.Background(), HandlerInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(follower.reasons[0], "Verdict: PROGRESSING") {
			t.Fatalf("expected PROGRESSING for recent activity, got: %s", follower.reasons[0])
		}
		if got := res.Result["progressing"]; got != 1 {
			t.Fatalf("progressing = %v, want 1", got)
		}
	})

	t.Run("stale member activity beyond interval is stalled", func(t *testing.T) {
		r := squadHeartbeatRow("aaa") // 30-minute interval
		stale := pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Hour), Valid: true}
		store := &fakeSquadHeartbeatStore{
			candidates: []db.ListSquadHeartbeatDueIssuesRow{r},
			issues:     map[pgtype.UUID]db.Issue{r.IssueID: {ID: r.IssueID}},
			telemetry: map[pgtype.UUID]db.GetSquadHeartbeatMemberActivityRow{
				r.IssueID: {RunningMemberTasks: 0, LastMemberTaskActivityAt: stale},
			},
		}
		follower := &fakeFollower{}
		handler := makeSquadHeartbeatInspectHandler(store, follower)

		if _, err := handler(context.Background(), HandlerInput{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(follower.reasons[0], "Verdict: STALLED") {
			t.Fatalf("expected STALLED for stale activity, got: %s", follower.reasons[0])
		}
	})

	t.Run("telemetry error falls back to wake-up with fallback note", func(t *testing.T) {
		r := squadHeartbeatRow("aaa")
		store := &fakeSquadHeartbeatStore{
			candidates:  []db.ListSquadHeartbeatDueIssuesRow{r},
			issues:      map[pgtype.UUID]db.Issue{r.IssueID: {ID: r.IssueID}},
			activityErr: map[pgtype.UUID]error{r.IssueID: errors.New("boom")},
		}
		follower := &fakeFollower{}
		handler := makeSquadHeartbeatInspectHandler(store, follower)

		res, err := handler(context.Background(), HandlerInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RowsAffected != 1 {
			t.Fatalf("RowsAffected = %d, want 1 (telemetry failure must not block wake-up)", res.RowsAffected)
		}
		if !strings.Contains(follower.reasons[0], "telemetry unavailable") {
			t.Fatalf("expected fallback note, got: %s", follower.reasons[0])
		}
	})
}

func TestSquadHeartbeatTelemetryStalled(t *testing.T) {
	now := time.Now()
	interval := 30 * time.Minute

	cases := []struct {
		name string
		tel  squadHeartbeatTelemetry
		want bool
	}{
		{
			name: "running task is never stalled",
			tel:  squadHeartbeatTelemetry{runningMemberTasks: 1, heartbeatInterval: interval},
			want: false,
		},
		{
			name: "no activity ever is stalled",
			tel:  squadHeartbeatTelemetry{runningMemberTasks: 0, heartbeatInterval: interval},
			want: true,
		},
		{
			name: "activity within interval is progressing",
			tel: squadHeartbeatTelemetry{
				runningMemberTasks:       0,
				lastMemberTaskActivityAt: pgtype.Timestamptz{Time: now.Add(-5 * time.Minute), Valid: true},
				heartbeatInterval:        interval,
			},
			want: false,
		},
		{
			name: "activity older than interval is stalled",
			tel: squadHeartbeatTelemetry{
				runningMemberTasks:       0,
				lastMemberTaskActivityAt: pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
				heartbeatInterval:        interval,
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tel.stalled(now); got != tc.want {
				t.Fatalf("stalled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSquadHeartbeatInspectJobSpec(t *testing.T) {
	store := &fakeSquadHeartbeatStore{}
	follower := &fakeFollower{}
	spec := SquadHeartbeatInspectJob(store, follower)

	if err := spec.validate(); err != nil {
		t.Fatalf("spec failed validation: %v", err)
	}
	if spec.Name != JobNameSquadHeartbeatInspect {
		t.Fatalf("Name = %q, want %q", spec.Name, JobNameSquadHeartbeatInspect)
	}
	if spec.Cadence <= 0 {
		t.Fatalf("Cadence must be > 0")
	}
	if spec.Handler == nil {
		t.Fatalf("Handler must be set")
	}
}
