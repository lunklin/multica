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
	if absent.Configured || absent.MaxRuns != nil || absent.RemainingRuns != nil || absent.ProjectID != project || absent.StopOnFailure || absent.FailureTaskID != nil {
		t.Fatalf("unconfigured project presented as a zero allowance: %+v", absent)
	}
	body := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	var initial projectRunBudgetResponse
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusOK).JSON(&initial)
	if initial.ProjectID != project || initial.WorkspaceID != ws || !initial.Configured || initial.RemainingRuns == nil || *initial.RemainingRuns != 6 || initial.Revision != 1 || initial.StopOnFailure || initial.FailureTaskID != nil {
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

func TestProjectRunBudgetFailurePolicyCompatibilityAndExplicitResume(t *testing.T) {
	ws, project, fixture := projectBudgetFixture(t)
	deadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := map[string]any{"max_runs": 6, "deadline": deadline, "expected_revision": 0, "stop_on_failure": true}
	var armed projectRunBudgetResponse
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, body)).Want(http.StatusOK).JSON(&armed)
	if !armed.StopOnFailure || armed.FailureTaskID != nil {
		t.Fatalf("failure-stop policy was not armed: %+v", armed)
	}
	runtime := fixture.Runtime(t, "failure policy runtime")
	agent := fixture.Agent(t, "failure policy agent", runtime)
	issue := fixture.Issue(t, "failed bounded task", testutil.Cols{"project_id": project})
	failed := fixture.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "status": "failed"})
	// Native failure transaction behavior is covered by the service tests. This
	// fixture isolates the API's authority to acknowledge an existing latch.
	if _, err := testPool.Exec(context.Background(), `UPDATE project_run_budget
		SET paused=true,failure_task_id=$1,revision=revision+1 WHERE project_id=$2`, failed, project); err != nil {
		t.Fatal(err)
	}
	var stopped projectRunBudgetResponse
	testutil.Call(t, testHandler.GetProjectRunBudget, projectBudgetRequest("GET", ws, project, nil)).Want(http.StatusOK).JSON(&stopped)
	if !stopped.Paused || stopped.StopReason != "failure_latch" || stopped.FailureTaskID == nil || *stopped.FailureTaskID != failed || stopped.AdmittedRuns != 1 || stopped.Revision != 2 {
		t.Fatalf("failure cause did not take precedence over paused: %+v", stopped)
	}
	for _, field := range []string{"failure_task_id", "stop_reason", "admitted_runs", "clear_failure_latch"} {
		invalid := map[string]any{"max_runs": 6, "deadline": deadline, "expected_revision": 2, field: nil}
		testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, invalid)).Want(http.StatusBadRequest)
	}
	for _, tc := range []struct {
		name       string
		fields     map[string]any
		wantPolicy bool
	}{
		{"old client omits policy and paused", nil, true},
		{"null policy preserves it", map[string]any{"stop_on_failure": nil}, true},
		{"disabling policy does not acknowledge failure", map[string]any{"stop_on_failure": false}, false},
		{"explicit pause preserves cause", map[string]any{"paused": true}, false},
		{"enabling policy still preserves cause", map[string]any{"stop_on_failure": true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := map[string]any{"max_runs": 6, "deadline": deadline, "expected_revision": stopped.Revision}
			for key, value := range tc.fields {
				update[key] = value
			}
			var next projectRunBudgetResponse
			testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, update)).Want(http.StatusOK).JSON(&next)
			if next.StopOnFailure != tc.wantPolicy || !next.Paused || next.FailureTaskID == nil || *next.FailureTaskID != failed || next.StopReason != "failure_latch" || next.AdmittedRuns != 1 || next.Revision != stopped.Revision+1 {
				t.Fatalf("update cleared policy/cause or refunded usage: %+v", next)
			}
			stopped = next
		})
	}
	resume := map[string]any{"max_runs": 6, "deadline": deadline, "expected_revision": stopped.Revision, "paused": false}
	var resumed projectRunBudgetResponse
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, resume)).Want(http.StatusOK).JSON(&resumed)
	if resumed.Paused || resumed.FailureTaskID != nil || resumed.StopReason != "" || !resumed.StopOnFailure || resumed.AdmittedRuns != 1 || resumed.MaxRuns == nil || *resumed.MaxRuns != 6 || resumed.RemainingRuns == nil || *resumed.RemainingRuns != 5 || resumed.Revision != stopped.Revision+1 {
		t.Fatalf("explicit human resume did not clear only the latch: %+v", resumed)
	}
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest("PUT", ws, project, resume)).Want(http.StatusConflict)
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
