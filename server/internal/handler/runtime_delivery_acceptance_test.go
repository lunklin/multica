package handler

import (
	"context"
	"net/http"
	"testing"
)

// Synthetic roles exercise the native handoff graph without invoking a model.
func TestRuntimeDeliveryWorkflowKeepsReviewReworkAndHumanCheckDistinct(t *testing.T) {
	seedTestCatalog(t)
	rt := dbfx.Runtime(t, "Delivery acceptance runtime")
	coordinator := dbfx.Agent(t, "Delivery coordinator", rt)
	engineer := dbfx.Agent(t, "Delivery engineer", rt)
	reviewer := dbfx.Agent(t, "Delivery reviewer", rt)
	humanName := "Human check " + workflowTestSuffix()[10:]
	wf := createTestWorkflowSteps(t, "Delivery acceptance "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"}, {"status_key": "in_progress"}, {"status_key": "in_review"},
		{"new_status": map[string]any{"name": humanName, "category": "started"}}, {"status_key": "blocked"},
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM issue_status WHERE workspace_id=$1 AND name=$2", testWorkspaceID, humanName)
	})
	humanKey := ""
	for _, step := range wf.Steps {
		if step.StatusKey != "backlog" && step.StatusKey != "todo" && step.StatusKey != "in_progress" && step.StatusKey != "in_review" && step.StatusKey != "blocked" && step.StatusKey != "done" && step.StatusKey != "cancelled" {
			humanKey = step.StatusKey
		}
	}
	if humanKey == "" {
		t.Fatal("human check status was not created")
	}
	setWorkflowHandoff(t, wf.ID, "todo", handToAgent(coordinator, "Decompose and dispatch ready work.")).Want(http.StatusOK)
	setWorkflowHandoff(t, wf.ID, "in_progress", handToAgent(engineer, "Implement and test the requested change.")).Want(http.StatusOK)
	reviewHandoff := handToAgent(reviewer, "Review the diff and return defects to engineering.")
	reviewHandoff["next_status_key"] = humanKey
	reviewHandoff["back_status_key"] = "in_progress"
	setWorkflowHandoff(t, wf.ID, "in_review", reviewHandoff).Want(http.StatusOK)
	for _, key := range []string{humanKey, "blocked"} {
		setWorkflowHandoff(t, wf.ID, key, map[string]any{"handler": map[string]any{"type": "member", "id": testUserID}}).Want(http.StatusOK)
	}
	project := createWorkflowTestProject(t, "Delivery acceptance project")
	setProjectWorkflow(t, project, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issue := createIssueForTest(t, map[string]any{"title": "Synthetic delivery", "project_id": project, "status": "todo"})
	assertStep := func(status, kind, id string) {
		t.Helper()
		got, at, aid := issueStatusAndAssignee(t, issue.ID)
		if got != status || at != kind || aid != id {
			t.Fatalf("step=%s assignee=%s/%s, want %s %s/%s", got, at, aid, status, kind, id)
		}
		var queued int
		dbfx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status='queued'", issue.ID).Scan(&queued)
		want := 0
		if kind == "agent" {
			want = 1
		}
		if queued != want {
			t.Fatalf("%s queued %d runs, want %d", status, queued, want)
		}
	}
	assertStep("todo", "agent", coordinator)
	for _, step := range []struct{ status, kind, id string }{
		{"in_progress", "agent", engineer}, {"in_review", "agent", reviewer},
		{"in_progress", "agent", engineer}, {"in_review", "agent", reviewer},
		{humanKey, "member", testUserID}, {"blocked", "member", testUserID},
		{humanKey, "member", testUserID},
	} {
		// Simulate role completion; only the handoff state machine is under test.
		dbfx.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status='queued'", issue.ID)
		updateIssueForTest(t, issue.ID, map[string]any{"status": step.status}).Want(http.StatusOK)
		assertStep(step.status, step.kind, step.id)
	}
	updateIssueForTest(t, issue.ID, map[string]any{"status": "done"}).Want(http.StatusOK)
	assertStep("done", "member", testUserID)
}
