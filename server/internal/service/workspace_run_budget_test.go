package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestWorkspaceRunBudgetRejectsSeventhRun(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	ws, _, agent, issue := seedAttributionFixture(t, pool)
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM workspace_run_budget WHERE workspace_id=$1", ws) })
	if _, err := pool.Exec(ctx, "INSERT INTO workspace_run_budget(workspace_id,max_runs,deadline) VALUES($1,6,clock_timestamp()+interval '1 hour')", ws); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 7; n++ {
		var id string
		err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue(agent_id,runtime_id,issue_id,status)
			SELECT id,runtime_id,$2,'completed' FROM agent WHERE id=$1 RETURNING id`, agent, issue).Scan(&id)
		if n <= 6 && err != nil {
			t.Fatalf("legitimate run %d refused: %v", n, err)
		}
		if n == 7 && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("seventh run admitted or returned unexpected error: id=%s err=%v", id, err)
		}
	}
}

func TestWorkspaceRunBudgetRejectsOversizedBatchAtomically(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	ws, _, agent, issue := seedAttributionFixture(t, pool)
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM workspace_run_budget WHERE workspace_id=$1", ws) })
	if _, err := pool.Exec(ctx, "INSERT INTO workspace_run_budget(workspace_id,max_runs,deadline) VALUES($1,1,clock_timestamp()+interval '1 hour')", ws); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO agent_task_queue(agent_id,runtime_id,issue_id,status)
		SELECT a.id,a.runtime_id,$2,'completed' FROM agent a CROSS JOIN generate_series(1,2) n WHERE a.id=$1`, agent, issue)
	if err == nil {
		t.Fatal("oversized batch was admitted")
	}
	b, err := db.New(pool).GetWorkspaceRunBudget(ctx, util.MustParseUUID(ws))
	if err != nil || b.AdmittedRuns != 0 {
		t.Fatalf("batch rollback spent allowance: %+v %v", b, err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE agent_id=$1", agent).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("partial batch survived: %d", count)
	}
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue(agent_id,runtime_id,status) SELECT id,runtime_id,'completed' FROM agent WHERE id=$1 RETURNING id`, agent).Scan(&id); err != nil {
		t.Fatalf("legitimate admission lost: %v", err)
	}
}

func TestWorkspaceRunBudgetArmingSerializesWithNativeEnqueue(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, agent, issue := seedAttributionFixture(t, pool)
	t.Cleanup(func() { pool.Exec(context.Background(), "DELETE FROM workspace_run_budget WHERE workspace_id=$1", ws) })
	q := db.New(pool)
	var runtime string
	if err := pool.QueryRow(ctx, "SELECT runtime_id::text FROM agent WHERE id=$1", agent).Scan(&runtime); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := q.WithTx(tx).LockWorkspaceForRunBudget(ctx, util.MustParseUUID(ws)); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	params := db.CreateAgentTaskParams{AgentID: util.MustParseUUID(agent), RuntimeID: util.MustParseUUID(runtime), IssueID: util.MustParseUUID(issue)}
	done := make(chan error, 1)
	go func() { _, err := db.New(conn).CreateAgentTask(ctx, params); done <- err }()
	waiting := false
	for n := 0; n < 100; n++ {
		if err := pool.QueryRow(ctx, "SELECT coalesce(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1", conn.Conn().PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("native enqueue did not wait for arming lock")
	}
	if _, err := tx.Exec(ctx, "INSERT INTO workspace_run_budget(workspace_id,max_runs,deadline) VALUES($1,1,clock_timestamp()+interval '1 hour')", ws); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("first post-arm run failed: %v", err)
	}
	b, err := q.GetWorkspaceRunBudget(ctx, util.MustParseUUID(ws))
	if err != nil || b.AdmittedRuns != 1 {
		t.Fatalf("enqueue escaped arming: %+v %v", b, err)
	}
}

func TestWorkspaceRunBudgetConcurrentAdmissions(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	ws, _, agent, issue := seedAttributionFixture(t, pool)
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM workspace_run_budget WHERE workspace_id=$1", ws) })
	if _, err := pool.Exec(ctx, "INSERT INTO workspace_run_budget(workspace_id,max_runs,deadline) VALUES($1,6,clock_timestamp()+interval '1 hour')", ws); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 18)
	for n := 0; n < 18; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var id string
			err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue(agent_id,runtime_id,issue_id,status)
				SELECT id,runtime_id,$2,'completed' FROM agent WHERE id=$1 RETURNING id`, agent, issue).Scan(&id)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	admitted, refused := 0, 0
	for err := range results {
		if err == nil {
			admitted++
		} else if errors.Is(err, pgx.ErrNoRows) {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	if admitted != 6 || refused != 12 {
		t.Fatalf("admitted=%d refused=%d, want 6/12", admitted, refused)
	}
	budget, err := db.New(pool).GetWorkspaceRunBudget(ctx, util.MustParseUUID(ws))
	if err != nil || budget.AdmittedRuns != 6 {
		t.Fatalf("counter=%d err=%v", budget.AdmittedRuns, err)
	}
}

func TestWorkspaceRunBudgetDoesNotRefundOrChargeConflicts(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	ws, _, agent, issue := seedAttributionFixture(t, pool)
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM workspace_run_budget WHERE workspace_id=$1", ws) })
	if _, err := pool.Exec(ctx, "INSERT INTO workspace_run_budget(workspace_id,max_runs,deadline) VALUES($1,2,clock_timestamp()+interval '1 hour')", ws); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO agent_task_queue(agent_id,runtime_id,issue_id,status)
		SELECT id,runtime_id,$2,'completed' FROM agent WHERE id=$1 RETURNING id`
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rolled string
	if err := tx.QueryRow(ctx, insert, agent, issue).Scan(&rolled); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var first string
	if err := pool.QueryRow(ctx, insert, agent, issue).Scan(&first); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO agent_task_queue(id,agent_id,runtime_id,status)
		SELECT $2,id,runtime_id,'completed' FROM agent WHERE id=$1 ON CONFLICT DO NOTHING`, agent, first)
	if err != nil {
		t.Fatal(err)
	}
	var second string
	if err := pool.QueryRow(ctx, insert, agent, issue).Scan(&second); err != nil {
		t.Fatalf("rollback/conflict spent a slot: %v", err)
	}
	for _, statement := range []string{
		"UPDATE agent_task_queue SET status='cancelled' WHERE id=$1",
		"UPDATE agent_task_queue SET status='queued' WHERE id=$1",
		"UPDATE agent_task_queue SET status='failed' WHERE id=$1",
		"DELETE FROM agent_task_queue WHERE id=$1",
	} {
		if _, err := pool.Exec(ctx, statement, first); err != nil {
			t.Fatal(err)
		}
	}
	var third string
	if err := pool.QueryRow(ctx, insert, agent, issue).Scan(&third); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lifecycle/deletion refunded a slot: %v", err)
	}
	b, err := db.New(pool).GetWorkspaceRunBudget(ctx, util.MustParseUUID(ws))
	if err != nil || b.AdmittedRuns != 2 {
		t.Fatalf("counter=%d err=%v", b.AdmittedRuns, err)
	}
}

func TestWorkspaceRunBudgetFailedParentCommitsWhenRetryRefused(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	ws, _, agent, issue := seedAttributionFixture(t, pool)
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM workspace_run_budget WHERE workspace_id=$1", ws) })
	if _, err := pool.Exec(ctx, "INSERT INTO workspace_run_budget(workspace_id,max_runs,deadline) VALUES($1,1,clock_timestamp()+interval '1 hour')", ws); err != nil {
		t.Fatal(err)
	}
	var parent string
	if err := pool.QueryRow(ctx, `INSERT INTO agent_task_queue(agent_id,runtime_id,issue_id,status,started_at,attempt,max_attempts)
		SELECT id,runtime_id,$2,'running',clock_timestamp(),1,2 FROM agent WHERE id=$1 RETURNING id`, agent, issue).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	if _, err := svc.FailTask(ctx, util.MustParseUUID(parent), "Connection closed mid-response.", "", "", "", "agent_error.provider_network", false, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := q.GetAgentTask(ctx, util.MustParseUUID(parent))
	if err != nil || got.Status != "failed" {
		t.Fatalf("parent terminal status lost: %s %v", got.Status, err)
	}
	var children int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1", parent).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 0 {
		t.Fatalf("retry exceeded budget: %d", children)
	}
}

func TestWorkspaceRunBudgetDeadlinePauseAndOtherWorkspace(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	ws, _, agent, issue := seedAttributionFixture(t, pool)
	_, _, other, otherIssue := seedAttributionFixture(t, pool)
	t.Cleanup(func() { pool.Exec(ctx, "DELETE FROM workspace_run_budget WHERE workspace_id=$1", ws) })
	if _, err := pool.Exec(ctx, "INSERT INTO workspace_run_budget(workspace_id,max_runs,deadline) VALUES($1,6,clock_timestamp()-interval '1 second')", ws); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO agent_task_queue(agent_id,runtime_id,issue_id,status) SELECT id,runtime_id,$2,'completed' FROM agent WHERE id=$1 RETURNING id`
	var id string
	if err := pool.QueryRow(ctx, insert, agent, issue).Scan(&id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired budget admitted: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE workspace_run_budget SET deadline=clock_timestamp()+interval '1 hour',paused=true WHERE workspace_id=$1", ws); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, insert, agent, issue).Scan(&id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("paused budget admitted: %v", err)
	}
	if err := pool.QueryRow(ctx, insert, other, otherIssue).Scan(&id); err != nil {
		t.Fatalf("unrelated workspace affected: %v", err)
	}
}
