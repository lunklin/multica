package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestProjectRunBudgetAgentDeleteSeesConcurrentArming(t *testing.T) {
	workspace, project, fixture := projectBudgetFixture(t)
	runtime := fixture.Runtime(t, "project deletion source runtime")
	agent := fixture.Agent(t, "owner project deletion source", runtime)
	task := fixture.Task(t, agent, testutil.Cols{"runtime_id": runtime, "status": "running"})
	req := projectBudgetRequest(http.MethodDelete, workspace, project, nil)
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	admin, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Rollback(context.Background())
	qtx := testHandler.Queries.WithTx(admin)
	if _, err := qtx.LockWorkspaceForRunBudget(ctx, parseUUID(workspace)); err != nil {
		t.Fatal(err)
	}
	if _, err := qtx.LockProjectForRunBudget(ctx, db.LockProjectForRunBudgetParams{WorkspaceID: parseUUID(workspace), ProjectID: parseUUID(project)}); err != nil {
		t.Fatal(err)
	}
	holderPID := int(admin.Conn().PgConn().PID())
	req = req.WithContext(ctx)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", agent)
	req.Header.Set("X-Task-ID", task)
	recorder := httptest.NewRecorder()
	entered, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(entered)
		testHandler.DeleteProject(recorder, req)
		close(done)
	}()
	<-entered
	defer func() {
		cancel()
		admin.Rollback(context.Background())
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("project delete handler did not stop after cancellation")
		}
	}()
	// Probe on the holder connection: only two pool connections are required.
	// The blocker PID and generated-query name identify this exact handler's
	// row-lock wait, rather than an unrelated test's database activity.
	probeDeadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := admin.Exec(ctx, "SELECT pg_stat_clear_snapshot()"); err != nil {
			t.Fatal(err)
		}
		var waiting bool
		if err := admin.QueryRow(ctx, `SELECT EXISTS (
            SELECT 1 FROM pg_stat_activity
            WHERE datname=current_database() AND state='active'
              AND wait_event_type='Lock' AND wait_event IN ('transactionid','tuple')
              AND query LIKE '-- name: LockProjectForDelete%'
              AND $1::int=ANY(pg_blocking_pids(pid))
        )`, holderPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-done:
			t.Fatalf("delete returned before reaching its project lock: status=%d body=%s", recorder.Code, recorder.Body.String())
		default:
		}
		if time.Now().After(probeDeadline) {
			t.Fatal("agent deletion never waited on the administrator's project lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := qtx.ConfigureProjectRunBudget(ctx, db.ConfigureProjectRunBudgetParams{
		WorkspaceID: parseUUID(workspace), ProjectID: parseUUID(project), MaxRuns: 6,
		Deadline: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("agent deletion did not finish after administrator commit")
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("agent removed a budget armed while waiting: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := testHandler.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: parseUUID(project), WorkspaceID: parseUUID(workspace)}); err != nil {
		t.Fatalf("project was lost: %v", err)
	}
	budget, err := testHandler.Queries.GetProjectRunBudget(ctx, db.GetProjectRunBudgetParams{WorkspaceID: parseUUID(workspace), ProjectID: parseUUID(project)})
	if err != nil || budget.MaxRuns != 6 || budget.AdmittedRuns != 0 || budget.Revision != 1 {
		t.Fatalf("concurrent budget was lost or changed: %+v, err=%v", budget, err)
	}
}

func TestProjectRunBudgetAgentOwnerCannotDeleteBudgetedProject(t *testing.T) {
	workspace, project, fixture := projectBudgetFixture(t)
	body := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest(http.MethodPut, workspace, project, body)).Want(http.StatusOK)
	runtime := fixture.Runtime(t, "bounded owner runtime")
	agent := fixture.Agent(t, "bounded owner agent", runtime)
	issue := fixture.Issue(t, "bounded project source", testutil.Cols{"project_id": project})
	task := fixture.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "status": "running"})
	req := projectBudgetRequest(http.MethodDelete, workspace, project, nil)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", agent)
	req.Header.Set("X-Task-ID", task)
	testutil.Call(t, testHandler.DeleteProject, req).Want(http.StatusForbidden)
	budget, err := testHandler.Queries.GetProjectRunBudget(context.Background(), db.GetProjectRunBudgetParams{WorkspaceID: parseUUID(workspace), ProjectID: parseUUID(project)})
	if err != nil || budget.AdmittedRuns != 1 || budget.MaxRuns != 6 {
		t.Fatalf("agent deletion removed or refunded allowance: %+v, err=%v", budget, err)
	}
}

func TestProjectRunBudgetHumanDeleteCleansBudget(t *testing.T) {
	workspace, project, _ := projectBudgetFixture(t)
	body := map[string]any{"max_runs": 6, "deadline": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "expected_revision": 0}
	testutil.Call(t, testHandler.ConfigureProjectRunBudget, projectBudgetRequest(http.MethodPut, workspace, project, body)).Want(http.StatusOK)
	testutil.Call(t, testHandler.DeleteProject, projectBudgetRequest(http.MethodDelete, workspace, project, nil)).Want(http.StatusNoContent)
	_, err := testHandler.Queries.GetProjectInWorkspace(context.Background(), db.GetProjectInWorkspaceParams{ID: parseUUID(project), WorkspaceID: parseUUID(workspace)})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("human project deletion did not remove project: %v", err)
	}
	_, err = testHandler.Queries.GetProjectRunBudget(context.Background(), db.GetProjectRunBudgetParams{WorkspaceID: parseUUID(workspace), ProjectID: parseUUID(project)})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("project deletion left a budget row: %v", err)
	}
}
