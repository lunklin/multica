package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func budgetFixture(t *testing.T) (string, *testutil.Fixture) {
	t.Helper()
	ctx := context.Background()
	var ws string
	if err := testPool.QueryRow(ctx, "INSERT INTO workspace(name,slug) VALUES('budget test',$1) RETURNING id", fmt.Sprintf("budget-%d", time.Now().UnixNano())).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM workspace_run_budget WHERE workspace_id=$1", ws)
		testPool.Exec(ctx, "DELETE FROM member WHERE workspace_id=$1", ws)
		testPool.Exec(ctx, "DELETE FROM workspace WHERE id=$1", ws)
	})
	if _, err := testPool.Exec(ctx, "INSERT INTO member(workspace_id,user_id,role) VALUES($1,$2,'owner')", ws, testUserID); err != nil {
		t.Fatal(err)
	}
	return ws, testutil.New(testPool, ws, testUserID)
}

func budgetRequest(method, ws string, body any) *http.Request {
	r := withURLParam(newRequest(method, "/api/workspaces/"+ws+"/run-budget", body), "id", ws)
	r.Header.Set("X-Workspace-ID", ws)
	return r
}

func TestWorkspaceRunBudgetAPIUsageAndHumanResume(t *testing.T) {
	ws, f := budgetFixture(t)
	var empty workspaceRunBudgetResponse
	testutil.Call(t, testHandler.GetWorkspaceRunBudget, budgetRequest("GET", ws, nil)).Want(http.StatusOK).JSON(&empty)
	if empty.Configured || empty.MaxRuns != nil || empty.RemainingRuns != nil {
		t.Fatalf("missing budget presented as zero allowance: %+v", empty)
	}
	body := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	var initial workspaceRunBudgetResponse
	testutil.Call(t, testHandler.ConfigureWorkspaceRunBudget, budgetRequest("PUT", ws, body)).Want(http.StatusOK).JSON(&initial)
	if initial.RemainingRuns == nil || *initial.RemainingRuns != 6 || initial.Revision != 1 {
		t.Fatalf("initial budget: %+v", initial)
	}
	rt := f.Runtime(t, "budget runtime")
	agent := f.Agent(t, "budget agent", rt)
	for n := 0; n < 6; n++ {
		f.Task(t, agent, testutil.Cols{"runtime_id": rt, "status": "completed"})
	}
	var spent workspaceRunBudgetResponse
	testutil.Call(t, testHandler.GetWorkspaceRunBudget, budgetRequest("GET", ws, nil)).Want(http.StatusOK).JSON(&spent)
	if spent.AdmittedRuns != 6 || spent.RemainingRuns == nil || *spent.RemainingRuns != 0 || spent.StopReason != "run_limit" {
		t.Fatalf("spent: %+v", spent)
	}
	agentReq := budgetRequest("PUT", ws, body)
	agentReq.Header.Set("X-Actor-Source", "task_token")
	testutil.Call(t, testHandler.ConfigureWorkspaceRunBudget, agentReq).Want(http.StatusForbidden)
	body["expected_revision"] = 1
	body["max_runs"] = 7
	var resumed workspaceRunBudgetResponse
	testutil.Call(t, testHandler.ConfigureWorkspaceRunBudget, budgetRequest("PUT", ws, body)).Want(http.StatusOK).JSON(&resumed)
	if resumed.AdmittedRuns != 6 || *resumed.RemainingRuns != 1 || resumed.Revision != 2 {
		t.Fatalf("resume reset usage: %+v", resumed)
	}
	testutil.Call(t, testHandler.ConfigureWorkspaceRunBudget, budgetRequest("PUT", ws, body)).Want(http.StatusConflict)
	body["expected_revision"] = 2
	body["max_runs"] = 5
	testutil.Call(t, testHandler.ConfigureWorkspaceRunBudget, budgetRequest("PUT", ws, body)).Want(http.StatusConflict)
	body["max_runs"] = 7
	body["admitted_runs"] = 0
	testutil.Call(t, testHandler.ConfigureWorkspaceRunBudget, budgetRequest("PUT", ws, body)).Want(http.StatusBadRequest)
}

func TestWorkspaceRunBudgetAPIRefusesArmingOverActiveRuns(t *testing.T) {
	ws, f := budgetFixture(t)
	rt := f.Runtime(t, "active runtime")
	agent := f.Agent(t, "active agent", rt)
	f.Task(t, agent, testutil.Cols{"runtime_id": rt})
	body := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	testutil.Call(t, testHandler.ConfigureWorkspaceRunBudget, budgetRequest("PUT", ws, body)).Want(http.StatusConflict)
}
