package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// This is a no-model HTTP state-machine test. Real role outputs and code
// quality are checked separately in the explicitly authorized live trial.
func TestBoundedDeliveryHandoffUsesSixAdmissions(t *testing.T) {
	seedTestCatalog(t)
	rt := dbfx.Runtime(t, "Bounded delivery runtime")
	mika := dbfx.Agent(t, "Bounded Mika", rt)
	engineer := dbfx.Agent(t, "Bounded engineer", rt)
	reviewer := dbfx.Agent(t, "Bounded reviewer", rt)
	names := []string{"Budget rework " + workflowTestSuffix(), "Budget delivery " + workflowTestSuffix(), "Budget human " + workflowTestSuffix()}
	wf := createTestWorkflowSteps(t, "Bounded six-run delivery "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"}, {"status_key": "in_progress"}, {"status_key": "in_review"},
		{"new_status": map[string]any{"name": names[0], "category": "started"}},
		{"new_status": map[string]any{"name": names[1], "category": "started"}},
		{"new_status": map[string]any{"name": names[2], "category": "started"}}, {"status_key": "blocked"},
	})
	keys := make([]string, 3)
	for n, name := range names {
		dbfx.QueryRow(t, "SELECT key FROM issue_status WHERE workspace_id=$1 AND name=$2", testWorkspaceID, name).Scan(&keys[n])
		t.Cleanup(func() {
			testPool.Exec(context.Background(), "DELETE FROM issue_status WHERE workspace_id=$1 AND name=$2", testWorkspaceID, name)
		})
	}
	rework, delivery, human := keys[0], keys[1], keys[2]
	for _, s := range []struct{ key, id, next, back string }{
		{"todo", mika, "in_progress", ""}, {"in_progress", engineer, "in_review", ""},
		{"in_review", reviewer, delivery, rework}, {rework, engineer, "in_review", ""}, {delivery, mika, human, ""},
	} {
		body := handToAgent(s.id, "Read the previous receipt and hand off once.")
		body["next_status_key"] = s.next
		if s.back != "" {
			body["back_status_key"] = s.back
		}
		setWorkflowHandoff(t, wf.ID, s.key, body).Want(http.StatusOK)
	}
	setWorkflowHandoff(t, wf.ID, human, map[string]any{"handler": map[string]any{"type": "member", "id": testUserID}}).Want(http.StatusOK)
	project := createWorkflowTestProject(t, "Bounded six-run project")
	setProjectWorkflow(t, project, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM workspace_run_budget WHERE workspace_id=$1", testWorkspaceID)
	})
	testutilBody := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	call := budgetRequest("PUT", testWorkspaceID, testutilBody)
	// The native API performs the no-active-runs check under its workspace lock.
	testutil.Call(t, testHandler.ConfigureWorkspaceRunBudget, call).Want(http.StatusOK)
	issue := createIssueForTest(t, map[string]any{"title": "Bounded handoff", "project_id": project, "status": "todo"})
	steps := []struct{ key, id string }{{"todo", mika}, {"in_progress", engineer}, {"in_review", reviewer}, {rework, engineer}, {"in_review", reviewer}, {delivery, mika}, {human, testUserID}}
	for n, s := range steps {
		if n > 0 {
			dbfx.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status='queued'", issue.ID)
			updateIssueForTest(t, issue.ID, map[string]any{"status": s.key}).Want(http.StatusOK)
		}
		status, _, owner := issueStatusAndAssignee(t, issue.ID)
		if status != s.key || owner != s.id {
			t.Fatalf("step %d: status=%s owner=%s", n+1, status, owner)
		}
		var count int
		dbfx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", issue.ID).Scan(&count)
		want := min(n+1, 6)
		if count != want {
			t.Fatalf("step %d created %d total tasks, want %d", n+1, count, want)
		}
	}
	// A further real handoff request cannot create a seventh queue row.
	updateIssueForTest(t, issue.ID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	var count int
	dbfx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1", issue.ID).Scan(&count)
	if count != 6 {
		t.Fatalf("extra handoff bypassed budget: %d", count)
	}
}
