package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeSquadHeartbeatStore stubs squadHeartbeatStore for handler tests. GetIssue
// mirrors fakeSquadHealthStore so the same "issue disappeared between scan and
// load" race can be exercised.
type fakeSquadHeartbeatStore struct {
	candidates []db.ListSquadHeartbeatDueIssuesRow
	issues     map[pgtype.UUID]db.Issue
	issueErr   map[pgtype.UUID]error
	listErr    error
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

func squadHeartbeatRow(id string) db.ListSquadHeartbeatDueIssuesRow {
	return db.ListSquadHeartbeatDueIssuesRow{
		SquadID:     pgtype.UUID{Bytes: byteID(id + "_squad"), Valid: true},
		LeaderID:    pgtype.UUID{Bytes: byteID(id + "_leader"), Valid: true},
		IssueID:     pgtype.UUID{Bytes: byteID(id + "_issue"), Valid: true},
		WorkspaceID: pgtype.UUID{Bytes: byteID(id + "_ws"), Valid: true},
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
