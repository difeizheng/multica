package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeSquadHealthStore stubs the squadHealthStore interface for handler tests.
type fakeSquadHealthStore struct {
	candidates []db.ListStalledSquadIssuesRow
	issues     map[pgtype.UUID]db.Issue
	// optional per-issue override: if set, GetIssue returns it instead of ErrNoRows
	issueErr map[pgtype.UUID]error
	listErr  error
}

func (f *fakeSquadHealthStore) ListStalledSquadIssues(ctx context.Context) ([]db.ListStalledSquadIssuesRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.candidates, nil
}

func (f *fakeSquadHealthStore) GetIssue(ctx context.Context, id pgtype.UUID) (db.Issue, error) {
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

// fakeFollower records every EnqueueSquadLeaderFollowUp call.
type fakeFollower struct {
	enqueued []pgtype.UUID // issue IDs that were enqueued
	reasons  []string      // handoff note passed per enqueue, parallel to enqueued
	failOn   map[pgtype.UUID]error
}

func (f *fakeFollower) EnqueueSquadLeaderFollowUp(ctx context.Context, issue db.Issue, triggeringAgentID pgtype.UUID, reason string) (bool, error) {
	if f.failOn != nil {
		if err, ok := f.failOn[issue.ID]; ok {
			return false, err
		}
	}
	f.enqueued = append(f.enqueued, issue.ID)
	f.reasons = append(f.reasons, reason)
	return true, nil
}

func squadHealthRow(id string) db.ListStalledSquadIssuesRow {
	return db.ListStalledSquadIssuesRow{
		SquadID:     pgtype.UUID{Bytes: byteID(id + "_squad"), Valid: true},
		LeaderID:    pgtype.UUID{Bytes: byteID(id + "_leader"), Valid: true},
		IssueID:     pgtype.UUID{Bytes: byteID(id + "_issue"), Valid: true},
		WorkspaceID: pgtype.UUID{Bytes: byteID(id + "_ws"), Valid: true},
	}
}

func byteID(s string) [16]byte {
	var b [16]byte
	copy(b[:], s)
	return b
}

func TestSquadHealthInspectHandler(t *testing.T) {
	t.Run("empty candidate list wakes no one", func(t *testing.T) {
		store := &fakeSquadHealthStore{candidates: nil, issues: map[pgtype.UUID]db.Issue{}}
		follower := &fakeFollower{}
		handler := makeSquadHealthInspectHandler(store, follower)

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

	t.Run("wakes one leader per stalled issue", func(t *testing.T) {
		r1 := squadHealthRow("aaa")
		r2 := squadHealthRow("bbb")
		store := &fakeSquadHealthStore{
			candidates: []db.ListStalledSquadIssuesRow{r1, r2},
			issues: map[pgtype.UUID]db.Issue{
				r1.IssueID: {ID: r1.IssueID},
				r2.IssueID: {ID: r2.IssueID},
			},
		}
		follower := &fakeFollower{}
		handler := makeSquadHealthInspectHandler(store, follower)

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
	})

	t.Run("missing issue (closed between scan and load) is skipped, not fatal", func(t *testing.T) {
		r1 := squadHealthRow("aaa")
		r2 := squadHealthRow("bbb")
		store := &fakeSquadHealthStore{
			candidates: []db.ListStalledSquadIssuesRow{r1, r2},
			issues: map[pgtype.UUID]db.Issue{
				// r2's issue disappeared (NOT in map -> GetIssue returns ErrNoRows)
				r1.IssueID: {ID: r1.IssueID},
			},
		}
		follower := &fakeFollower{}
		handler := makeSquadHealthInspectHandler(store, follower)

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
		r1 := squadHealthRow("aaa")
		r2 := squadHealthRow("bbb")
		store := &fakeSquadHealthStore{
			candidates: []db.ListStalledSquadIssuesRow{r1, r2},
			issues: map[pgtype.UUID]db.Issue{
				r1.IssueID: {ID: r1.IssueID},
				r2.IssueID: {ID: r2.IssueID},
			},
		}
		follower := &fakeFollower{
			failOn: map[pgtype.UUID]error{r1.IssueID: errors.New("boom")},
		}
		handler := makeSquadHealthInspectHandler(store, follower)

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
		store := &fakeSquadHealthStore{listErr: errors.New("db down")}
		follower := &fakeFollower{}
		handler := makeSquadHealthInspectHandler(store, follower)

		if _, err := handler(context.Background(), HandlerInput{}); err == nil {
			t.Fatalf("expected error from list failure")
		}
	})
}

func TestSquadHealthInspectJobSpec(t *testing.T) {
	store := &fakeSquadHealthStore{}
	follower := &fakeFollower{}
	spec := SquadHealthInspectJob(store, follower)

	if err := spec.validate(); err != nil {
		t.Fatalf("spec failed validation: %v", err)
	}
	if spec.Name != JobNameSquadHealthInspect {
		t.Fatalf("Name = %q, want %q", spec.Name, JobNameSquadHealthInspect)
	}
	if spec.Cadence <= 0 {
		t.Fatalf("Cadence must be > 0")
	}
	if spec.Handler == nil {
		t.Fatalf("Handler must be set")
	}
}
