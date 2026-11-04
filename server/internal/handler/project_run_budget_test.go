package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func projectBudgetFixture(t *testing.T) (string, string, *testutil.Fixture) {
	t.Helper()
	ws, fixture := budgetFixture(t)
	project := fixture.Project(t, "project run budget")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM project_run_budget WHERE project_id=$1", project)
	})
	return ws, project, fixture
}

func projectBudgetRequest(method, workspace, project string, body any) *http.Request {
	r := withURLParam(newRequest(method, "/api/projects/"+project+"/run-budget", body), "id", project)
	r.Header.Set("X-Workspace-ID", workspace)
	return r
}

func TestProjectRunBudgetAPIUsageAndHumanResume(t *testing.T) {
	ws, project, fixture := projectBudgetFixture(t)
	other := fixture.Project(t, "unrelated project")
	runtime := fixture.Runtime(t, "project budget runtime")
	agent := fixture.Agent(t, "project budget agent", runtime)
	otherIssue := fixture.Issue(t, "unrelated active work", testutil.Cols{"project_id": other})
	fixture.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": otherIssue})

	var absent projectRunBudgetResponse
	testutil.Call(t, testHandler.GetProjectRunBudget, projectBudgetRequest("GET", ws, project, nil)).Want(http.StatusOK).JSON(&absent)
	if absent.Configured || absent.MaxRuns != nil || absent.RemainingRuns != nil || absent.ProjectID != project {
		t.Fatalf("unconfigured project presented as a zero allowance: %+v", absent)
	}
	body := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	var initial projectRunBudgetResponse
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusOK).JSON(&initial)
	if initial.ProjectID != project || initial.WorkspaceID != ws || !initial.Configured || initial.RemainingRuns == nil || *initial.RemainingRuns != 6 || initial.Revision != 1 {
		t.Fatalf("initial project budget: %+v", initial)
	}
	issue := fixture.Issue(t, "bounded project work", testutil.Cols{"project_id": project})
	for n := 0; n < 6; n++ {
		fixture.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "status": "completed"})
	}
	var spent projectRunBudgetResponse
	testutil.Call(t, testHandler.GetProjectRunBudget, projectBudgetRequest("GET", ws, project, nil)).Want(http.StatusOK).JSON(&spent)
	if spent.AdmittedRuns != 6 || spent.RemainingRuns == nil || *spent.RemainingRuns != 0 || spent.StopReason != "run_limit" {
		t.Fatalf("project admissions were not charged: %+v", spent)
	}
	fixture.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": otherIssue, "status": "completed"})
	var unaffected projectRunBudgetResponse
	testutil.Call(t, testHandler.GetProjectRunBudget, projectBudgetRequest("GET", ws, other, nil)).Want(http.StatusOK).JSON(&unaffected)
	if unaffected.Configured || unaffected.MaxRuns != nil {
		t.Fatalf("unrelated project inherited this allowance: %+v", unaffected)
	}
	body["max_runs"], body["expected_revision"] = 7, 1
	var resumed projectRunBudgetResponse
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusOK).JSON(&resumed)
	if resumed.AdmittedRuns != 6 || *resumed.RemainingRuns != 1 || resumed.Revision != 2 {
		t.Fatalf("human extension reset or lost charged usage: %+v", resumed)
	}
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusConflict)
	body["max_runs"], body["expected_revision"] = 5, 2
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusConflict)
	body["max_runs"], body["paused"] = 7, true
	var paused projectRunBudgetResponse
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusOK).JSON(&paused)
	if paused.StopReason != "paused" || paused.AdmittedRuns != 6 || paused.Revision != 3 {
		t.Fatalf("pause did not preserve durable admissions: %+v", paused)
	}
	body["expected_revision"], body["admitted_runs"] = 3, 0
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusBadRequest)
}

func TestProjectRunBudgetAPIRefusesArmingOverOwnedActiveRuns(t *testing.T) {
	for _, kind := range []string{"issue", "chat"} {
		t.Run(kind, func(t *testing.T) {
			ws, project, fixture := projectBudgetFixture(t)
			runtime := fixture.Runtime(t, "active project runtime")
			agent := fixture.Agent(t, "active project agent", runtime)
			cols := testutil.Cols{"runtime_id": runtime}
			if kind == "chat" {
				cols["chat_session_id"] = fixture.ChatSession(t, agent, testutil.Cols{"project_id": project})
			} else {
				cols["issue_id"] = fixture.Issue(t, "active bounded issue", testutil.Cols{"project_id": project})
			}
			fixture.Task(t, agent, cols)
			body := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
			testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusConflict)
		})
	}
}

func TestProjectRunBudgetAPIDoesNotChargeLaterProjectSelection(t *testing.T) {
	ws, project, fixture := projectBudgetFixture(t)
	runtime := fixture.Runtime(t, "unscoped project runtime")
	agent := fixture.Agent(t, "unscoped project agent", runtime)
	session := fixture.ChatSession(t, agent)
	fixture.Task(t, agent, testutil.Cols{"runtime_id": runtime, "chat_session_id": session})
	if _, err := testPool.Exec(context.Background(), "UPDATE chat_session SET project_id=$1 WHERE id=$2", project, session); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	var budget projectRunBudgetResponse
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusOK).JSON(&budget)
	if budget.AdmittedRuns != 0 {
		t.Fatalf("post-admission project selection retroactively charged work: %+v", budget)
	}
}
