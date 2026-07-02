package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

type runningSquadLeaderTaskFixture struct {
	IssueID          string
	LeaderID         string
	TaskID           string
	TriggerCommentID string
}

func newRunningSquadLeaderTaskFixture(t *testing.T) runningSquadLeaderTaskFixture {
	t.Helper()
	ctx := context.Background()

	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)

	var runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT runtime_id FROM agent WHERE id = $1
	`, fx.LeaderID).Scan(&runtimeID); err != nil {
		t.Fatalf("load leader runtime: %v", err)
	}

	var triggerCommentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'member', $3, 'LGTM', 'comment')
		RETURNING id
	`, issueID, testWorkspaceID, testUserID).Scan(&triggerCommentID); err != nil {
		t.Fatalf("create trigger comment: %v", err)
	}

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, issue_id, trigger_comment_id,
			status, priority, started_at
		)
		VALUES ($1, $2, $3, $4, 'running', 0, now())
		RETURNING id
	`, fx.LeaderID, runtimeID, issueID, triggerCommentID).Scan(&taskID); err != nil {
		t.Fatalf("create running squad leader task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})

	return runningSquadLeaderTaskFixture{
		IssueID:          issueID,
		LeaderID:         fx.LeaderID,
		TaskID:           taskID,
		TriggerCommentID: triggerCommentID,
	}
}

func recordSquadLeaderEvaluationForTask(t *testing.T, fx runningSquadLeaderTaskFixture, outcome string) {
	t.Helper()
	recordSquadLeaderEvaluationForTaskWithHeader(t, fx, outcome, fx.TaskID)
}

func recordSquadLeaderEvaluationForTaskWithHeader(t *testing.T, fx runningSquadLeaderTaskFixture, outcome, taskIDHeader string) {
	t.Helper()

	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/issues/"+fx.IssueID+"/squad-evaluated", map[string]any{
		"outcome": outcome,
		"reason":  "test reason",
	})
	r = withURLParam(r, "id", fx.IssueID)
	r.Header.Set("X-Agent-ID", fx.LeaderID)
	r.Header.Set("X-Task-ID", taskIDHeader)

	testHandler.RecordSquadLeaderEvaluation(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("RecordSquadLeaderEvaluation: expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func completeRunningTask(t *testing.T, fx runningSquadLeaderTaskFixture, output string) {
	t.Helper()

	w := httptest.NewRecorder()
	r := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+fx.TaskID+"/complete",
		map[string]any{"output": output},
		testWorkspaceID, "legit-daemon")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", fx.TaskID)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	testHandler.CompleteTask(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("CompleteTask: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func countAgentCommentsForIssue(t *testing.T, issueID, agentID string) int {
	t.Helper()
	var count int
	if err := testPool.QueryRow(context.Background(), `
		SELECT count(*) FROM comment
		WHERE issue_id = $1 AND author_type = 'agent' AND author_id = $2
	`, issueID, agentID).Scan(&count); err != nil {
		t.Fatalf("count agent comments: %v", err)
	}
	return count
}

// latestSquadLeaderEvaluationDetails loads the most recent squad_leader_evaluated
// activity row for an issue and returns its JSONB details as a map. Used to
// assert on the `verified` flag written by RecordSquadLeaderEvaluation.
func latestSquadLeaderEvaluationDetails(t *testing.T, issueID string) map[string]any {
	t.Helper()
	var details []byte
	if err := testPool.QueryRow(context.Background(), `
		SELECT details FROM activity_log
		WHERE issue_id = $1 AND action = 'squad_leader_evaluated'
		ORDER BY created_at DESC, id DESC LIMIT 1
	`, issueID).Scan(&details); err != nil {
		t.Fatalf("load latest squad_leader_evaluated activity: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(details, &m); err != nil {
		t.Fatalf("unmarshal activity details: %v", err)
	}
	return m
}

// insertMemberTaskOnChildIssue simulates the downstream effect of a real
// leader dispatch: a child issue assigned to a member agent, plus the
// agent_task_queue row that the assignment enqueues. The row's created_at
// lands inside the leader task's [started_at, now] window so the verification
// count query picks it up.
func insertMemberTaskOnChildIssue(t *testing.T, parentIssueID, memberAgentID string) {
	t.Helper()
	ctx := context.Background()

	var runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT runtime_id FROM agent WHERE id = $1
	`, memberAgentID).Scan(&runtimeID); err != nil {
		t.Fatalf("load member runtime: %v", err)
	}

	var childID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title,
		                   assignee_type, assignee_id, parent_issue_id)
		VALUES ($1, 'member', $2, 'child dispatch', 'agent', $3, $4)
		RETURNING id
	`, testWorkspaceID, testUserID, memberAgentID, parentIssueID).Scan(&childID); err != nil {
		t.Fatalf("create child issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, childID)
	})

	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
		VALUES ($1, $2, $3, 'queued', 0)
	`, memberAgentID, runtimeID, childID); err != nil {
		t.Fatalf("create member task on child issue: %v", err)
	}
}

// TestRecordSquadLeaderEvaluation_ActionWithoutDispatchIsUnverified encodes
// the core fix: a leader that records `action` without spawning any member
// task this turn must be marked verified=false so the Inspections panel can
// distinguish a real dispatch from a self-reported coordinative turn.
func TestRecordSquadLeaderEvaluation_ActionWithoutDispatchIsUnverified(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newRunningSquadLeaderTaskFixture(t)

	recordSquadLeaderEvaluationForTask(t, fx, "action")

	details := latestSquadLeaderEvaluationDetails(t, fx.IssueID)
	got, ok := details["verified"]
	if !ok {
		t.Fatalf("expected verified key on action evaluation details, got %v", details)
	}
	if got != false {
		t.Fatalf("expected verified=false for action with no member dispatch, got %v", got)
	}
}

// TestRecordSquadLeaderEvaluation_ActionWithChildIssueDispatchIsVerified
// confirms that a real dispatch (a child issue assigned to a non-leader
// member, which enqueues a member agent_task_queue row in the task window)
// marks the evaluation verified=true.
func TestRecordSquadLeaderEvaluation_ActionWithChildIssueDispatchIsVerified(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newRunningSquadLeaderTaskFixture(t)
	memberID := createHandlerTestAgent(t, "Verified Dispatch Member", nil)
	// Make the member a squad member so the dispatch is realistic; the
	// verification query only filters by agent_id <> leader_id, but keeping
	// the squad roster honest avoids surprising future readers.
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO squad_member (squad_id, member_type, member_id, role)
		SELECT squad.assignee_id, 'agent', $1, '' FROM issue WHERE id = $2
	`, memberID, fx.IssueID); err != nil {
		t.Fatalf("add squad member: %v", err)
	}

	insertMemberTaskOnChildIssue(t, fx.IssueID, memberID)
	recordSquadLeaderEvaluationForTask(t, fx, "action")

	details := latestSquadLeaderEvaluationDetails(t, fx.IssueID)
	if got := details["verified"]; got != true {
		t.Fatalf("expected verified=true when a member task exists in window, got %v", got)
	}
}

// TestRecordSquadLeaderEvaluation_NoActionHasNoVerifiedFlag ensures the
// verified flag is only attached to outcome=action rows — no_action and
// failed outcomes leave it absent so legacy readers treat them as verified.
func TestRecordSquadLeaderEvaluation_NoActionHasNoVerifiedFlag(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newRunningSquadLeaderTaskFixture(t)

	recordSquadLeaderEvaluationForTask(t, fx, "no_action")

	details := latestSquadLeaderEvaluationDetails(t, fx.IssueID)
	if _, ok := details["verified"]; ok {
		t.Fatalf("expected no verified key on no_action evaluation, got %v", details)
	}
}

func TestCompleteTask_SquadLeaderNoActionDoesNotSynthesizeComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)
	recordSquadLeaderEvaluationForTask(t, fx, "no_action")

	completeRunningTask(t, fx, "No action needed. Exiting silently.")

	if got := countAgentCommentsForIssue(t, fx.IssueID, fx.LeaderID); got != 0 {
		t.Fatalf("expected no squad leader comment after no_action completion, got %d", got)
	}
}

func TestCompleteTask_SquadLeaderNoActionCanonicalizesTaskID(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)
	recordSquadLeaderEvaluationForTaskWithHeader(t, fx, "no_action", strings.ToUpper(fx.TaskID))

	completeRunningTask(t, fx, "No action needed. Exiting silently.")

	if got := countAgentCommentsForIssue(t, fx.IssueID, fx.LeaderID); got != 0 {
		t.Fatalf("expected no comment when no_action was recorded with uppercase task id header, got %d", got)
	}
}

func TestCompleteTask_SquadLeaderActionStillSynthesizesComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)
	recordSquadLeaderEvaluationForTask(t, fx, "action")

	completeRunningTask(t, fx, "Delegated the review.")

	if got := countAgentCommentsForIssue(t, fx.IssueID, fx.LeaderID); got != 1 {
		t.Fatalf("expected action completion to synthesize one comment, got %d", got)
	}
}

func TestCreateComment_SquadLeaderNoActionRejectsComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	fx := newRunningSquadLeaderTaskFixture(t)
	recordSquadLeaderEvaluationForTask(t, fx, "no_action")

	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/issues/"+fx.IssueID+"/comments", map[string]any{
		"content":   "No action needed.",
		"parent_id": fx.TriggerCommentID,
	})
	r = withURLParam(r, "id", fx.IssueID)
	r.Header.Set("X-Agent-ID", fx.LeaderID)
	r.Header.Set("X-Task-ID", fx.TaskID)

	testHandler.CreateComment(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("CreateComment: expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if got := countAgentCommentsForIssue(t, fx.IssueID, fx.LeaderID); got != 0 {
		t.Fatalf("expected rejected no_action comment not to be stored, got %d", got)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body["error"] == "" {
		t.Fatalf("expected error message in response, got %v", body)
	}
}
