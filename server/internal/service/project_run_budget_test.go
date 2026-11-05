package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func seedProjectRunBudget(t *testing.T, maxRuns int) (*pgxpool.Pool, string, string, string, string) {
	t.Helper()
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	ws, _, agent, issue := seedAttributionFixture(t, pool)
	var project string
	if err := pool.QueryRow(ctx, "INSERT INTO project(workspace_id,title) VALUES($1,'bounded synthetic project') RETURNING id", ws).Scan(&project); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE issue SET project_id=$1 WHERE id=$2", project, issue); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO project_run_budget(project_id,workspace_id,max_runs,deadline) VALUES($1,$2,$3,clock_timestamp()+interval '1 hour')", project, ws, maxRuns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM project_run_budget WHERE workspace_id=$1", ws)
		pool.Exec(context.Background(), "DELETE FROM workspace_run_budget WHERE workspace_id=$1", ws)
	})
	return pool, ws, agent, issue, project
}

func insertProjectBudgetTask(ctx context.Context, pool *pgxpool.Pool, agent string, issue, source any) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue(agent_id,runtime_id,issue_id,delegated_from_task_id,status)
 SELECT id,runtime_id,$2,$3,'completed' FROM agent WHERE id=$1 RETURNING id`, agent, issue, source).Scan(&id)
	return id, err
}

func TestProjectRunBudgetConcurrentAdmissions(t *testing.T) {
	pool, _, agent, issue, project := seedProjectRunBudget(t, 6)
	var allowed, refused atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 18; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := insertProjectBudgetTask(context.Background(), pool, agent, issue, nil)
			if err == nil {
				allowed.Add(1)
			} else if errors.Is(err, pgx.ErrNoRows) {
				refused.Add(1)
			} else {
				t.Errorf("enqueue: %v", err)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 6 || refused.Load() != 12 {
		t.Fatalf("allowed=%d refused=%d", allowed.Load(), refused.Load())
	}
	var used int
	if err := pool.QueryRow(context.Background(), "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used); err != nil || used != 6 {
		t.Fatalf("used=%d err=%v", used, err)
	}
}

func TestProjectRunBudgetCrossProjectDescendantsChargeBothScopes(t *testing.T) {
	pool, ws, agent, issue, project := seedProjectRunBudget(t, 2)
	ctx := context.Background()
	parent, err := insertProjectBudgetTask(ctx, pool, agent, issue, nil)
	if err != nil {
		t.Fatal(err)
	}
	var destination string
	if err := pool.QueryRow(ctx, "INSERT INTO project(workspace_id,title) VALUES($1,'independent destination') RETURNING id", ws).Scan(&destination); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO project_run_budget(project_id,workspace_id,max_runs,deadline) VALUES($1,$2,3,clock_timestamp()+interval '1 hour')", destination, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE issue SET project_id=$1 WHERE id=$2", destination, issue); err != nil {
		t.Fatal(err)
	}
	child, err := insertProjectBudgetTask(ctx, pool, agent, issue, parent)
	if err != nil {
		t.Fatal(err)
	}
	var origins, targets int
	var snapshots bool
	if err := pool.QueryRow(ctx, "SELECT $2::uuid=ANY(budget_project_ids) AND $3::uuid=ANY(budget_project_ids) FROM agent_task_queue WHERE id=$1", child, project, destination).Scan(&snapshots); err != nil || !snapshots {
		t.Fatalf("missing source/destination scopes: %v %v", snapshots, err)
	}
	if err := pool.QueryRow(ctx, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&origins); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", destination).Scan(&targets); err != nil {
		t.Fatal(err)
	}
	if origins != 2 || targets != 1 {
		t.Fatalf("source=%d destination=%d", origins, targets)
	}
	if _, err := insertProjectBudgetTask(ctx, pool, agent, nil, child); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unscoped descendant escaped origin budget: %v", err)
	}
	if _, err := insertProjectBudgetTask(ctx, pool, agent, issue, nil); err != nil {
		t.Fatalf("unrelated fresh work was blocked: %v", err)
	}
}

func TestProjectRunBudgetSnapshotsCannotBeReplacedOrRefunded(t *testing.T) {
	pool, _, agent, issue, project := seedProjectRunBudget(t, 2)
	ctx := context.Background()
	parent, err := insertProjectBudgetTask(ctx, pool, agent, issue, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE agent_task_queue SET budget_project_ids='{}'::uuid[] WHERE id=$1", parent); err == nil {
		t.Fatal("snapshot cleared")
	}
	child, err := insertProjectBudgetTask(ctx, pool, agent, nil, parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM agent_task_queue WHERE id=$1", parent); err != nil {
		t.Fatal(err)
	}
	if _, err := insertProjectBudgetTask(ctx, pool, agent, nil, child); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted ancestor lost budget: %v", err)
	}
	var used int
	if err := pool.QueryRow(ctx, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used); err != nil || used != 2 {
		t.Fatalf("used=%d err=%v", used, err)
	}
}

func TestProjectRunBudgetWorkspaceAndProjectAreAtomic(t *testing.T) {
	for _, limits := range []struct {
		name               string
		project, workspace int
	}{{"project first", 1, 6}, {"workspace first", 6, 1}} {
		t.Run(limits.name, func(t *testing.T) {
			pool, ws, agent, issue, project := seedProjectRunBudget(t, limits.project)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, "INSERT INTO workspace_run_budget(workspace_id,max_runs,deadline) VALUES($1,$2,clock_timestamp()+interval '1 hour')", ws, limits.workspace); err != nil {
				t.Fatal(err)
			}
			if _, err := insertProjectBudgetTask(ctx, pool, agent, issue, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := insertProjectBudgetTask(ctx, pool, agent, issue, nil); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("excess admitted: %v", err)
			}
			var projectUsed, workspaceUsed int
			if err := pool.QueryRow(ctx, "SELECT p.admitted_runs,w.admitted_runs FROM project_run_budget p JOIN workspace_run_budget w ON w.workspace_id=p.workspace_id WHERE p.project_id=$1", project).Scan(&projectUsed, &workspaceUsed); err != nil {
				t.Fatal(err)
			}
			if projectUsed != 1 || workspaceUsed != 1 {
				t.Fatalf("partial charge: project=%d workspace=%d", projectUsed, workspaceUsed)
			}
		})
	}
}

func TestProjectRunBudgetRejectsBatchAndRollsBackCharges(t *testing.T) {
	pool, _, agent, issue, project := seedProjectRunBudget(t, 1)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO agent_task_queue(agent_id,runtime_id,issue_id,status)
 SELECT a.id,a.runtime_id,$2,'completed' FROM agent a CROSS JOIN generate_series(1,2) n WHERE a.id=$1`, agent, issue)
	if err == nil {
		t.Fatal("oversized batch admitted")
	}
	var used, rows int
	if err := pool.QueryRow(ctx, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE agent_id=$1", agent).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if used != 0 || rows != 0 {
		t.Fatalf("batch partially committed: used=%d rows=%d", used, rows)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO agent_task_queue(agent_id,runtime_id,issue_id,status) SELECT id,runtime_id,$2,'completed' FROM agent WHERE id=$1", agent, issue); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := insertProjectBudgetTask(ctx, pool, agent, issue, nil); err != nil {
		t.Fatalf("rollback spent allowance: %v", err)
	}
}

func TestProjectRunBudgetDeadlineAndPause(t *testing.T) {
	pool, _, agent, issue, project := seedProjectRunBudget(t, 2)
	ctx := context.Background()
	for _, change := range []string{"paused=true", "paused=false,deadline=clock_timestamp()-interval '1 second'"} {
		if _, err := pool.Exec(ctx, "UPDATE project_run_budget SET "+change+" WHERE project_id=$1", project); err != nil {
			t.Fatal(err)
		}
		if _, err := insertProjectBudgetTask(ctx, pool, agent, issue, nil); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("stopped budget admitted: %v", err)
		}
	}
	var used int
	if err := pool.QueryRow(ctx, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used); err != nil || used != 0 {
		t.Fatalf("used=%d err=%v", used, err)
	}
}

func TestProjectRunBudgetRecoveryPreservesCallerAndFailedScopes(t *testing.T) {
	pool, ws, agent, issue, sourceProject := seedProjectRunBudget(t, 3)
	ctx := context.Background()
	caller, err := insertProjectBudgetTask(ctx, pool, agent, issue, nil)
	if err != nil {
		t.Fatal(err)
	}
	var failedProject string
	if err := pool.QueryRow(ctx, "INSERT INTO project(workspace_id,title) VALUES($1,'failed work project') RETURNING id", ws).Scan(&failedProject); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO project_run_budget(project_id,workspace_id,max_runs,deadline) VALUES($1,$2,3,clock_timestamp()+interval '1 hour')", failedProject, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE issue SET project_id=$1 WHERE id=$2", failedProject, issue); err != nil {
		t.Fatal(err)
	}
	failed, err := insertProjectBudgetTask(ctx, pool, agent, issue, nil)
	if err != nil {
		t.Fatal(err)
	}
	var recovery string
	if err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue(agent_id,runtime_id,delegated_from_task_id,trigger_evidence_kind,trigger_evidence_ref_id,status)
 SELECT id,runtime_id,$2,'delegated_failure',$3,'completed' FROM agent WHERE id=$1 RETURNING id`, agent, caller, failed).Scan(&recovery); err != nil {
		t.Fatal(err)
	}
	var inherited bool
	if err := pool.QueryRow(ctx, "SELECT $2::uuid=ANY(budget_project_ids) AND $3::uuid=ANY(budget_project_ids) FROM agent_task_queue WHERE id=$1", recovery, sourceProject, failedProject).Scan(&inherited); err != nil || !inherited {
		t.Fatalf("recovery lost failed-origin scope: inherited=%v err=%v", inherited, err)
	}
}
