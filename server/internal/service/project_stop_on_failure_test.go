package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func seedStopOnFailure(t *testing.T) (*delegatedFailureFixture, *TaskService, *dbfx.Fixture, string) {
	t.Helper()
	f, svc, fx, project := seedWorkflowRecovery(t, "coordinator", false)
	fx.Exec(t, "UPDATE project_run_budget SET stop_on_failure=true WHERE project_id=$1", project)
	return f, svc, fx, project
}

func TestProjectStopOnFailureNativeFailTask(t *testing.T) {
	for _, policy := range []string{"enabled", "disabled", "unconfigured"} {
		for _, reason := range []string{"agent_error.provider_network", "agent_error.process_failure"} {
			t.Run(policy+"/"+reason, func(t *testing.T) {
				f, svc, fx, project := seedStopOnFailure(t)
				if policy == "disabled" {
					fx.Exec(t, "UPDATE project_run_budget SET stop_on_failure=false WHERE project_id=$1", project)
				}
				if policy == "unconfigured" {
					fx.Exec(t, "DELETE FROM project_run_budget WHERE project_id=$1", project)
				}
				id := f.insertWorkerTask(t, "running", "comment", 1, 2)
				failed, err := svc.FailTask(context.Background(), id, "synthetic runtime failure", "", "", "", reason, false, "", "")
				if err != nil || failed.Status != "failed" {
					t.Fatalf("failure was lost: %+v %v", failed, err)
				}
				var retries, recoveries int
				fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1", id).Scan(&retries)
				fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", id).Scan(&recoveries)
				if policy == "enabled" {
					b, err := svc.Queries.GetProjectRunBudget(context.Background(), db.GetProjectRunBudgetParams{WorkspaceID: util.MustParseUUID(f.workspaceID), ProjectID: util.MustParseUUID(project)})
					if err != nil || !b.Paused || b.FailureTaskID != id || b.AdmittedRuns != 1 || b.Revision != 2 || retries != 0 || recoveries != 0 {
						t.Fatalf("failure did not atomically stop: budget=%+v retries=%d recovery=%d err=%v", b, retries, recoveries, err)
					}
					// Replays neither charge nor replace the first failure receipt.
					if _, err := svc.FailTask(context.Background(), id, "replayed failure", "", "", "", reason, false, "", ""); err != nil {
						t.Fatal(err)
					}
					b2, err := svc.Queries.GetProjectRunBudget(context.Background(), db.GetProjectRunBudgetParams{WorkspaceID: b.WorkspaceID, ProjectID: b.ProjectID})
					if err != nil || b2.Revision != b.Revision || b2.FailureTaskID != b.FailureTaskID || b2.AdmittedRuns != 1 {
						t.Fatalf("replay changed latch: %+v %v", b2, err)
					}
					if reason == "agent_error.process_failure" {
						var signals int
						fx.QueryRow(t, "SELECT count(*) FROM comment WHERE source_task_id=$1 AND author_type='system' AND type='progress_update'", id).Scan(&signals)
						if signals != 1 {
							t.Fatalf("failure signal lost: %d", signals)
						}
					}
				} else {
					wantRetry, wantRecovery := 1, 0
					if reason == "agent_error.process_failure" {
						wantRetry, wantRecovery = 0, 1
					}
					if retries != wantRetry || recoveries != wantRecovery {
						t.Fatalf("opt-out changed: retries=%d recoveries=%d", retries, recoveries)
					}
				}
			})
		}
	}
}

func TestProjectStopOnFailureHoldsPreparedTasksUntilHumanResume(t *testing.T) {
	f, svc, fx, project := seedStopOnFailure(t)
	ctx := context.Background()
	failedID := f.insertWorkerTask(t, "running", "comment", 1, 2)
	queued := fx.Task(t, f.worker, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "queued"})
	prepared := fx.Task(t, f.coordinator, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "dispatched", "dispatched_at": time.Now()})
	active := fx.Task(t, f.coordinator, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "running", "dispatched_at": time.Now(), "started_at": time.Now()})
	p, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(prepared))
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(active))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FailTask(ctx, failedID, "synthetic failure", "", "", "", "agent_error.provider_network", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.ClaimTask(ctx, util.MustParseUUID(f.worker)); err != nil || got != nil {
		t.Fatalf("latched queue claimed: %+v %v", got, err)
	}
	if _, err := svc.StartTask(ctx, p.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("legacy start bypassed latch: %v", err)
	}
	claim := db.LockAgentTaskStartClaimParams{ID: p.ID, RuntimeID: p.RuntimeID, DispatchedAt: p.DispatchedAt}
	if _, err := svc.StartTaskForClaim(ctx, claim); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim start bypassed latch: %v", err)
	}
	if _, err := svc.Queries.StartAgentTaskWithSupplement(ctx, db.StartAgentTaskWithSupplementParams{TaskID: p.ID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("native start bypassed latch: %v", err)
	}
	replay, err := svc.StartTaskForClaim(ctx, db.LockAgentTaskStartClaimParams{ID: a.ID, RuntimeID: a.RuntimeID, DispatchedAt: a.DispatchedAt})
	if err != nil || replay.Status != "running" {
		t.Fatalf("running ACK replay was killed: %+v %v", replay, err)
	}
	b, err := svc.Queries.GetProjectRunBudget(ctx, db.GetProjectRunBudgetParams{WorkspaceID: util.MustParseUUID(f.workspaceID), ProjectID: util.MustParseUUID(project)})
	if err != nil {
		t.Fatal(err)
	}
	if b.AdmittedRuns != 4 {
		t.Fatalf("unexpected admissions: %d", b.AdmittedRuns)
	}
	// A cap/policy edit cannot implicitly clear a failure latch.
	b, err = svc.Queries.UpdateProjectRunBudget(ctx, db.UpdateProjectRunBudgetParams{WorkspaceID: b.WorkspaceID, ProjectID: b.ProjectID, MaxRuns: 6, Deadline: b.Deadline, ExpectedRevision: b.Revision, Paused: false, StopOnFailure: pgtype.Bool{Bool: false, Valid: true}})
	if err != nil || !b.Paused || !b.FailureTaskID.Valid {
		t.Fatalf("implicit unpause erased failure: %+v %v", b, err)
	}
	b, err = svc.Queries.UpdateProjectRunBudget(ctx, db.UpdateProjectRunBudgetParams{WorkspaceID: b.WorkspaceID, ProjectID: b.ProjectID, MaxRuns: 6, Deadline: b.Deadline, ExpectedRevision: b.Revision, Paused: false, ClearFailureLatch: true})
	if err != nil || b.Paused || b.FailureTaskID.Valid || b.AdmittedRuns != 4 {
		t.Fatalf("explicit resume refunded or failed: %+v %v", b, err)
	}
	if _, err := svc.StartTaskForClaim(ctx, claim); err != nil {
		t.Fatal("held prepare did not resume:", err)
	}
	var status string
	fx.QueryRow(t, "SELECT status FROM agent_task_queue WHERE id=$1", queued).Scan(&status)
	if status != "queued" {
		t.Fatal("held queue was cancelled")
	}
}

func TestProjectStopOnFailureSuccessAndLastAdmittedRunStillStart(t *testing.T) {
	f, svc, fx, project := seedStopOnFailure(t)
	ctx := context.Background()
	fx.Exec(t, "UPDATE project_run_budget SET max_runs=2 WHERE project_id=$1", project)
	first := fx.Task(t, f.worker, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "running"})
	// A successful role RETURN, including text that says tests failed, remains success.
	if _, err := svc.CompleteTask(ctx, util.MustParseUUID(first), []byte(`{"receipt":"RETURN: expected red test failed; rework requested"}`), "", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	next, err := svc.Queries.CreateAgentTask(ctx, db.CreateAgentTaskParams{AgentID: util.MustParseUUID(f.coordinator), RuntimeID: util.MustParseUUID(f.runtimeID), IssueID: util.MustParseUUID(f.workerIssue), DelegatedFromTaskID: util.MustParseUUID(first)})
	if err != nil {
		t.Fatal("successful handoff rejected:", err)
	}
	fx.Exec(t, "UPDATE agent_task_queue SET status='dispatched',dispatched_at=now() WHERE id=$1", next.ID)
	// The admission counter is now full; this already-admitted run must start.
	if _, err := svc.StartTask(ctx, next.ID); err != nil {
		t.Fatal("last admission could not start:", err)
	}
	b, err := svc.Queries.GetProjectRunBudget(ctx, db.GetProjectRunBudgetParams{WorkspaceID: util.MustParseUUID(f.workspaceID), ProjectID: util.MustParseUUID(project)})
	if err != nil || b.Paused || b.FailureTaskID.Valid || b.AdmittedRuns != 2 {
		t.Fatalf("success latched project: %+v %v", b, err)
	}
}

// Wait for an actual PostgreSQL row-lock wait, rather than treating elapsed
// time as evidence that another connection could not cross the fence.
func waitProjectFailureLock(t *testing.T, ctx context.Context, f *delegatedFailureFixture, pid int, result <-chan error) {
	t.Helper()
	for {
		var waiting bool
		if err := f.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case err := <-result:
			t.Fatalf("operation crossed lock fence: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestProjectStopOnFailureFailureWinsStartRace(t *testing.T) {
	f, svc, fx, project := seedStopOnFailure(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	failed := f.insertWorkerTask(t, "running", "comment", 1, 2)
	prepared := fx.Task(t, f.coordinator, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "dispatched", "dispatched_at": time.Now()})
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	qtx := svc.Queries.WithTx(tx)
	if _, err := qtx.FailAgentTask(ctx, db.FailAgentTaskParams{ID: failed, Error: pgtype.Text{String: "failure wins", Valid: true}, FailureReason: pgtype.Text{String: "agent_error.provider_network", Valid: true}}); err != nil {
		t.Fatal(err)
	}
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := db.New(conn).StartAgentTaskWithSupplement(ctx, db.StartAgentTaskWithSupplementParams{TaskID: util.MustParseUUID(prepared)})
		result <- err
	}()
	waitProjectFailureLock(t, ctx, f, pid, result)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("prepare started after failure commit: %v", err)
	}
	b, err := svc.Queries.GetProjectRunBudget(ctx, db.GetProjectRunBudgetParams{WorkspaceID: util.MustParseUUID(f.workspaceID), ProjectID: util.MustParseUUID(project)})
	if err != nil || b.FailureTaskID != failed || b.AdmittedRuns != 2 {
		t.Fatalf("latch or usage lost: %+v %v", b, err)
	}
}

func TestProjectStopOnFailureStartWinsFailureRace(t *testing.T) {
	f, svc, fx, _ := seedStopOnFailure(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	failed := f.insertWorkerTask(t, "running", "comment", 1, 2)
	prepared := fx.Task(t, f.coordinator, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "dispatched", "dispatched_at": time.Now()})
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := svc.Queries.WithTx(tx).StartAgentTaskWithSupplement(ctx, db.StartAgentTaskWithSupplementParams{TaskID: util.MustParseUUID(prepared)}); err != nil {
		t.Fatal(err)
	}
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := db.New(conn).FailAgentTask(ctx, db.FailAgentTaskParams{ID: failed, Error: pgtype.Text{String: "later failure", Valid: true}})
		result <- err
	}()
	waitProjectFailureLock(t, ctx, f, pid, result)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	var status string
	fx.QueryRow(t, "SELECT status FROM agent_task_queue WHERE id=$1", prepared).Scan(&status)
	if status != "running" {
		t.Fatal("in-flight run was killed")
	}
}

func TestProjectStopOnFailureScopeAndFirstFailureHistory(t *testing.T) {
	f, svc, fx, sourceProject := seedStopOnFailure(t)
	ctx := context.Background()
	_, _, otherfx, otherProject := seedStopOnFailure(t)
	independent := fx.Project(t, "independent project")
	fx.Exec(t, "INSERT INTO project_run_budget(project_id,workspace_id,max_runs,deadline,stop_on_failure) VALUES($1,$2,6,now()+interval '1 hour',true)", independent, f.workspaceID)
	fx.Cleanup(t, "DELETE FROM project_run_budget WHERE project_id=$1", independent)
	independentIssue := fx.Issue(t, "independent issue", dbfx.Cols{"project_id": independent})
	independentTask := fx.Task(t, f.worker, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": independentIssue, "status": "dispatched", "dispatched_at": time.Now()})
	destination := fx.Project(t, "synthetic destination")
	fx.Exec(t, "INSERT INTO project_run_budget(project_id,workspace_id,max_runs,deadline,stop_on_failure) VALUES($1,$2,6,now()+interval '1 hour',true)", destination, f.workspaceID)
	fx.Cleanup(t, "DELETE FROM project_run_budget WHERE project_id=$1", destination)
	source := fx.Task(t, f.coordinator, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.issueID, "status": "completed"})
	fx.Exec(t, "UPDATE issue SET project_id=$2 WHERE id=$1", f.workerIssue, destination)
	failed := util.MustParseUUID(fx.Task(t, f.worker, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "delegated_from_task_id": source, "status": "running"}))
	later := util.MustParseUUID(fx.Task(t, f.coordinator, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "delegated_from_task_id": source, "status": "running"}))
	if _, err := svc.FailTask(ctx, failed, "cross-project failure", "", "", "", "agent_error.process_failure", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FailTask(ctx, later, "another in-flight task failed", "", "", "", "agent_error.process_failure", false, "", ""); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{sourceProject, destination} {
		b, err := svc.Queries.GetProjectRunBudget(ctx, db.GetProjectRunBudgetParams{WorkspaceID: util.MustParseUUID(f.workspaceID), ProjectID: util.MustParseUUID(project)})
		if err != nil || b.FailureTaskID != failed || !b.Paused {
			t.Fatalf("scope %s escaped: %+v %v", project, b, err)
		}
	}
	if _, err := svc.StartTask(ctx, util.MustParseUUID(independentTask)); err != nil {
		t.Fatal("unrelated project was stopped:", err)
	}
	// History is independent of task deletion and usage is never refunded.
	fx.Exec(t, "DELETE FROM agent_task_queue WHERE id=$1", failed)
	var receipt string
	fx.QueryRow(t, "SELECT failure_task_id::text FROM project_run_budget WHERE project_id=$1", sourceProject).Scan(&receipt)
	if receipt != util.UUIDToString(failed) {
		t.Fatal("failure receipt lost after task retention")
	}
	var paused bool
	otherfx.QueryRow(t, "SELECT paused FROM project_run_budget WHERE project_id=$1", otherProject).Scan(&paused)
	if paused {
		t.Fatal("foreign tenant was paused")
	}
}

func TestProjectStopOnFailureNativeFailureQueries(t *testing.T) {
	for _, path := range []string{"orphans", "stale", "runtime_offline", "queued_expired", "reconnect_expired", "cleanup_unconfirmed"} {
		t.Run(path, func(t *testing.T) {
			f, svc, fx, project := seedStopOnFailure(t)
			ctx := context.Background()
			status := "running"
			switch path {
			case "stale":
				status = "dispatched"
			case "queued_expired":
				status = "queued"
			case "reconnect_expired":
				status = "deferred"
			case "cleanup_unconfirmed":
				status = "cancelled"
			}
			cols := dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": status, "created_at": time.Now().Add(-time.Hour), "dispatched_at": time.Now().Add(-time.Hour), "prepare_lease_expires_at": time.Now().Add(-time.Hour), "fire_at": time.Now().Add(-time.Hour)}
			if path == "reconnect_expired" {
				parent := fx.Task(t, f.worker, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "failed", "failure_reason": "runtime_offline"})
				cols["parent_task_id"] = parent
			}
			id := util.MustParseUUID(fx.Task(t, f.worker, cols))
			if path == "runtime_offline" || path == "queued_expired" || path == "reconnect_expired" {
				fx.Exec(t, "UPDATE agent_runtime SET status='offline',last_seen_at=now()-interval '1 hour',updated_at=now()-interval '1 hour' WHERE id=$1", f.runtimeID)
			}
			var rows []db.AgentTaskQueue
			var err error
			switch path {
			case "orphans":
				rows, err = svc.RecoverOrphanedTasksForRuntime(ctx, util.MustParseUUID(f.runtimeID))
			case "stale":
				rows, err = svc.FailStaleTasks(ctx, db.FailStaleTasksParams{DispatchTimeoutSecs: 1, RunningTimeoutSecs: 1, RuntimeStaleSecs: 60, RuntimeReconnectGraceSecs: 60})
			case "runtime_offline":
				rows, err = svc.FailTasksForOfflineRuntimes(ctx, db.FailTasksForOfflineRuntimesParams{ReconnectGraceSecs: 1, MaxPerTick: 10})
			case "queued_expired":
				rows, err = svc.ExpireStaleQueuedTasks(ctx, db.ExpireStaleQueuedTasksParams{ReconnectGraceSecs: 1, MaxPerTick: 10})
			case "reconnect_expired":
				rows, err = svc.FailExpiredRuntimeReconnectRetries(ctx, db.FailExpiredRuntimeReconnectRetriesParams{ReconnectGraceSecs: 1, RuntimeStaleSecs: 60, MaxPerTick: 10})
			case "cleanup_unconfirmed":
				var row db.FailCancelledTaskProcessCleanupRow
				row, err = svc.Queries.FailCancelledTaskProcessCleanup(ctx, db.FailCancelledTaskProcessCleanupParams{ID: id, ErrorMessage: "synthetic cleanup failure"})
				rows = []db.AgentTaskQueue{db.AgentTaskQueue(row)}
			}
			found := false
			for _, row := range rows {
				if row.ID == id && row.Status == "failed" {
					found = true
				}
			}
			// Sweepers are intentionally global; unrelated synthetic rows left by
			// another test may also qualify. Assert this fixture's exact CAS result.
			if err != nil || !found {
				t.Fatalf("native %s failure lost: rows=%+v err=%v", path, rows, err)
			}
			b, err := svc.Queries.GetProjectRunBudget(ctx, db.GetProjectRunBudgetParams{WorkspaceID: util.MustParseUUID(f.workspaceID), ProjectID: util.MustParseUUID(project)})
			if err != nil || b.FailureTaskID != id || !b.Paused {
				t.Fatalf("native %s bypassed latch: %+v %v", path, b, err)
			}
			if path != "cleanup_unconfirmed" {
				svc.HandleFailedTasks(ctx, rows)
				var children int
				fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1", id).Scan(&children)
				if children != 0 {
					t.Fatal("bulk failure admitted retry")
				}
			}
		})
	}
}

func TestProjectStopOnFailureBudgetLocksPrecedeTaskLocks(t *testing.T) {
	for _, operation := range []string{"fail", "start", "start_claim"} {
		t.Run(operation, func(t *testing.T) {
			f, svc, fx, project := seedStopOnFailure(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			id := util.MustParseUUID(fx.Task(t, f.worker, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "dispatched", "dispatched_at": time.Now()}))
			task, err := svc.Queries.GetAgentTask(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			budgetTx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer budgetTx.Rollback(context.Background())
			if _, err := budgetTx.Exec(ctx, "SELECT project_id FROM project_run_budget WHERE project_id=$1 FOR UPDATE", project); err != nil {
				t.Fatal(err)
			}
			conn, err := f.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			var pid int
			if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				q := db.New(conn)
				var err error
				switch operation {
				case "fail":
					_, err = q.FailAgentTask(ctx, db.FailAgentTaskParams{ID: id, Error: pgtype.Text{String: "synthetic failure", Valid: true}})
				case "start":
					_, err = q.StartAgentTaskWithSupplement(ctx, db.StartAgentTaskWithSupplementParams{TaskID: id})
				case "start_claim":
					_, err = q.LockAgentTaskStartClaim(ctx, db.LockAgentTaskStartClaimParams{ID: id, RuntimeID: task.RuntimeID, DispatchedAt: task.DispatchedAt})
				}
				result <- err
			}()
			waitProjectFailureLock(t, ctx, f, pid, result)
			probeTx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer probeTx.Rollback(context.Background())
			if _, err := probeTx.Exec(ctx, "SELECT id FROM agent_task_queue WHERE id=$1 FOR UPDATE NOWAIT", id); err != nil {
				t.Fatalf("%s took task lock before its budget fence: %v", operation, err)
			}
			if err := probeTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := budgetTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}
