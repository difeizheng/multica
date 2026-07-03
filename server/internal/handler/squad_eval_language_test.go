package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// squadEvalLanguageFixture creates a squad assigned to an issue whose title is
// Chinese, plus a running leader task on it — the minimal setup needed to
// exercise the reason-language enforcement in RecordSquadLeaderEvaluation.
type squadEvalLanguageFixture struct {
	IssueID  string
	LeaderID string
	TaskID   string
}

func newSquadEvalLanguageFixture(t *testing.T, title string) squadEvalLanguageFixture {
	t.Helper()
	ctx := context.Background()

	var leaderID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&leaderID); err != nil {
		t.Fatalf("load leader agent: %v", err)
	}

	var squadID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, description, leader_id, creator_id)
		VALUES ($1, 'Eval Language Squad', '', $2, $3)
		RETURNING id
	`, testWorkspaceID, leaderID, testUserID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
	})

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title, assignee_type, assignee_id)
		VALUES ($1, 'member', $2, $3, 'squad', $4)
		RETURNING id
	`, testWorkspaceID, testUserID, title, squadID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	var runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT runtime_id FROM agent WHERE id = $1
	`, leaderID).Scan(&runtimeID); err != nil {
		t.Fatalf("load leader runtime: %v", err)
	}

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at)
		VALUES ($1, $2, $3, 'running', 0, now())
		RETURNING id
	`, leaderID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("create running leader task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})

	return squadEvalLanguageFixture{IssueID: issueID, LeaderID: leaderID, TaskID: taskID}
}

// postEval records (or attempts to record) a squad leader evaluation and
// returns the HTTP status code without failing on non-2xx — the language test
// asserts a 422 rejection.
func postEval(t *testing.T, fx squadEvalLanguageFixture, outcome, reason string) int {
	t.Helper()

	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/issues/"+fx.IssueID+"/squad-evaluated", map[string]any{
		"outcome": outcome,
		"reason":  reason,
	})
	r = withURLParam(r, "id", fx.IssueID)
	r.Header.Set("X-Agent-ID", fx.LeaderID)
	r.Header.Set("X-Task-ID", fx.TaskID)

	testHandler.RecordSquadLeaderEvaluation(w, r)
	return w.Code
}

// TestRecordSquadLeaderEvaluation_RejectsEnglishReasonOnChineseIssue encodes
// the server-side language enforcement: a CJK-titled issue must not accept an
// English (zero-CJK) reason. Prompt-level rules were ignored by the LLM under
// English-heavy context, so the check lives at the write boundary and covers
// every trigger path.
func TestRecordSquadLeaderEvaluation_RejectsEnglishReasonOnChineseIssue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadEvalLanguageFixture(t, "基于技术方案文档开发系统")

	if code := postEval(t, fx, "no_action", "All members completed work; blocked on infrastructure."); code != http.StatusUnprocessableEntity {
		t.Fatalf("English reason on Chinese issue: expected 422, got %d", code)
	}

	if code := postEval(t, fx, "no_action", "全部成员已完成工作，当前因基础设施阻塞"); code != http.StatusCreated {
		t.Fatalf("Chinese reason on Chinese issue: expected 201, got %d", code)
	}
}

// TestRecordSquadLeaderEvaluation_AllowsEnglishReasonOnEnglishIssue confirms
// the enforcement is keyed to the issue's language: English-titled issues are
// not forced to use Chinese.
func TestRecordSquadLeaderEvaluation_AllowsEnglishReasonOnEnglishIssue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadEvalLanguageFixture(t, "Implement the backend API")

	if code := postEval(t, fx, "no_action", "Members still working on the task."); code != http.StatusCreated {
		t.Fatalf("English reason on English issue: expected 201, got %d", code)
	}
}

func TestHasCJK(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"基于技术方案文档开发系统", true},
		{"Developer API 500 错误", true},
		{"", false},
		{"All members completed work.", false},
		{"基于技术方案", true},
	}
	for _, c := range cases {
		if got := hasCJK(c.in); got != c.want {
			t.Errorf("hasCJK(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
