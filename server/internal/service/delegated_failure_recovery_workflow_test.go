package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

func seedWorkflowRecovery(t *testing.T, owner string, defaultWorkflow bool) (*delegatedFailureFixture, *TaskService, *dbfx.Fixture, string) {
	t.Helper()
	f, svc := seedDelegatedFailureFixture(t)
	fx := dbfx.New(f.pool, f.workspaceID, f.userID)
	project := fx.Project(t, "Synthetic recovery project")
	steps := []issueworkflow.Step{
		{StatusKey: "todo"},
		{StatusKey: "in_progress", Handler: issueworkflow.Handler{Type: "agent", ID: f.worker}, Instructions: "Engineer owns the repair."},
		{StatusKey: "in_review", Handler: issueworkflow.Handler{Type: "agent", ID: f.coordinator}},
		{StatusKey: "done"}, {StatusKey: "cancelled"},
	}
	if defaultWorkflow {
		for n := range steps {
			steps[n].Handler = issueworkflow.Handler{Type: "none"}
			steps[n].Instructions = ""
		}
	}
	raw, err := issueworkflow.EncodeSteps(steps)
	if err != nil {
		t.Fatal(err)
	}
	workflow := fx.Insert(t, "issue_workflow", dbfx.Cols{"workspace_id": f.workspaceID, "owner_id": f.userID, "name": "Synthetic recovery workflow", "initial_status_key": "todo", "steps": raw, "is_default": defaultWorkflow})
	if !defaultWorkflow {
		fx.Exec(t, "UPDATE project SET workflow_id=$2 WHERE id=$1", project, workflow)
	}
	assigneeType, assigneeID := "agent", f.worker
	switch owner {
	case "coordinator":
		assigneeID = f.coordinator
	case "squad", "other_squad_leader":
		leader := f.coordinator
		if owner == "other_squad_leader" {
			leader = f.worker
		}
		squad := fx.Squad(t, "Synthetic recovery squad", leader)
		assigneeType, assigneeID = "squad", squad
		fx.Exec(t, "UPDATE agent_task_queue SET squad_id=$2,is_leader_task=true WHERE id=$1", f.sourceTask, squad)
	case "member":
		assigneeType, assigneeID = "member", f.userID
	}
	fx.Exec(t, "UPDATE issue SET project_id=$2,assignee_type=$3,assignee_id=$4 WHERE id=$1", f.issueID, project, assigneeType, assigneeID)
	fx.Exec(t, "UPDATE issue SET project_id=$2 WHERE id=$1", f.workerIssue, project)
	fx.Exec(t, "INSERT INTO project_run_budget(project_id,workspace_id,max_runs,deadline) VALUES($1,$2,6,$3)", project, f.workspaceID, time.Now().Add(time.Hour))
	fx.Cleanup(t, "DELETE FROM project_run_budget WHERE project_id=$1", project)
	return f, svc, fx, project
}

func TestDelegatedFailureWorkflowObserverStopsWithoutRunOrBudgetCharge(t *testing.T) {
	for _, owner := range []string{"engineer", "member", "other_squad_leader"} {
		t.Run(owner, func(t *testing.T) {
			f, svc, fx, project := seedWorkflowRecovery(t, owner, false)
			ctx := context.Background()
			failedID := f.insertWorkerTask(t, "failed", "comment", 1, 1)
			fx.Exec(t, "UPDATE agent_task_queue SET issue_id=$2,failure_reason=$3,error='provider returned HTTP 429',completed_at=now() WHERE id=$1", failedID, f.issueID, taskfailure.ReasonAgentProviderCapacityOrRateLimit.String())
			// An independently queued, legitimate handler task must not be cancelled or rewritten.
			pending := fx.Task(t, f.worker, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.issueID, "status": "queued"})
			var beforeState string
			fx.QueryRow(t, "SELECT jsonb_build_array(status,assignee_type,assignee_id)::text FROM issue WHERE id=$1", f.issueID).Scan(&beforeState)
			target, created, err := svc.ensureDelegatedFailureRecoveryComment(ctx, failedID)
			if err != nil || target == nil || !created {
				t.Fatalf("seed native failure signal: %v %v", created, err)
			}
			original := target.comment.Content
			failed, err := svc.Queries.GetAgentTask(ctx, failedID)
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for n := 0; n < 4; n++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if handled, err := svc.recoverDelegatedTaskFailure(ctx, failed); err != nil || !handled {
						t.Errorf("repeat recovery handled=%v err=%v", handled, err)
					}
				}()
			}
			wg.Wait()
			var queue, used, notices, inboxCount int
			fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", failedID).Scan(&queue)
			fx.QueryRow(t, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used)
			fx.QueryRow(t, "SELECT count(*) FROM comment WHERE workspace_id=$1 AND issue_id=$2 AND source_task_id=$3 AND type='system'", f.workspaceID, f.issueID, failedID).Scan(&notices)
			fx.QueryRow(t, "SELECT count(*) FROM inbox_item WHERE workspace_id=$1 AND recipient_type='member' AND recipient_id=$2 AND details->>'outcome'='needs_human' AND details->>'failed_task_id'=$3", f.workspaceID, f.userID, util.UUIDToString(failedID)).Scan(&inboxCount)
			if queue != 0 || used != 2 || notices != 1 || inboxCount != 1 {
				t.Fatalf("observer escalated incorrectly: recovery_runs=%d used=%d notices=%d inbox=%d", queue, used, notices, inboxCount)
			}
			var afterState, pendingStatus, signal string
			var settled bool
			// Native comment inserts advance the issue revision; business ownership does not move.
			fx.QueryRow(t, "SELECT jsonb_build_array(status,assignee_type,assignee_id)::text FROM issue WHERE id=$1", f.issueID).Scan(&afterState)
			fx.QueryRow(t, "SELECT status FROM agent_task_queue WHERE id=$1", pending).Scan(&pendingStatus)
			fx.QueryRow(t, "SELECT content,recovery_settled_at IS NOT NULL FROM comment WHERE id=$1", target.comment.ID).Scan(&signal, &settled)
			if afterState != beforeState || pendingStatus != "queued" || signal != original || !settled {
				t.Fatalf("history/ownership changed: state=%s want=%s pending=%s settled=%v signal=%s", afterState, beforeState, pendingStatus, settled, signal)
			}
			stored, err := svc.Queries.GetAgentTask(ctx, failedID)
			if err != nil || stored.Status != "failed" || stored.Error.String != "provider returned HTTP 429" {
				t.Fatalf("failed task evidence changed: %v %v", stored.Status, err)
			}
			if result, err := svc.RecoverPendingDelegatedFailures(ctx, 100); err != nil || result.Scanned != 0 {
				t.Fatalf("settled observer signal was replayed: %+v %v", result, err)
			}
			// Changing the owner later does not reopen a terminal needs_human decision.
			fx.Exec(t, "UPDATE issue SET assignee_type='agent',assignee_id=$2 WHERE id=$1", f.issueID, f.coordinator)
			if handled, err := svc.recoverDelegatedTaskFailure(ctx, failed); err != nil || !handled {
				t.Fatalf("settled replay: %v %v", handled, err)
			}
			// A concurrent callback may already hold the original unsettled snapshot.
			if _, err := svc.dispatchDelegatedFailureRecovery(ctx, target, pgtype.UUID{}); err != nil {
				t.Fatalf("stale unsettled replay: %v", err)
			}
			fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", failedID).Scan(&queue)
			if queue != 0 {
				t.Fatal("later owner change resurrected stopped recovery")
			}
		})
	}
}

func TestDelegatedFailureWorkflowEffectiveOwnerContinues(t *testing.T) {
	for _, owner := range []string{"coordinator", "squad"} {
		t.Run(owner, func(t *testing.T) {
			f, svc, fx, project := seedWorkflowRecovery(t, owner, false)
			ctx := context.Background()
			failedID := f.insertWorkerTask(t, "running", "comment", 1, 1)
			failed, err := svc.FailTask(ctx, failedID, "provider returned HTTP 429", "", "", "", taskfailure.ReasonAgentProviderCapacityOrRateLimit.String(), false, "", "")
			if err != nil {
				t.Fatal(err)
			}
			var runs, used, inboxCount int
			var actor string
			fx.QueryRow(t, "SELECT count(*),COALESCE(max(agent_id::text),'') FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", failedID).Scan(&runs, &actor)
			fx.QueryRow(t, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used)
			fx.QueryRow(t, "SELECT count(*) FROM inbox_item WHERE workspace_id=$1 AND details->>'outcome'='needs_human'", f.workspaceID).Scan(&inboxCount)
			if runs != 1 || actor != f.coordinator || used != 2 || inboxCount != 0 {
				t.Fatalf("legal override owner was blocked: runs=%d actor=%s used=%d inbox=%d", runs, actor, used, inboxCount)
			}
			if handled, err := svc.recoverDelegatedTaskFailure(ctx, *failed); err != nil || !handled {
				t.Fatalf("repeat legitimate recovery: %v %v", handled, err)
			}
			if owner == "squad" {
				var squad pgtype.UUID
				fx.QueryRow(t, "SELECT squad_id FROM agent_task_queue WHERE trigger_evidence_ref_id=$1 AND trigger_evidence_kind='delegated_failure'", failedID).Scan(&squad)
				if !squad.Valid {
					t.Fatal("legal squad recovery lost squad projection")
				}
			}
		})
	}
}

func TestDelegatedFailureWorkflowFailedRepairRequestsHumanOnce(t *testing.T) {
	f, svc, fx, project := seedWorkflowRecovery(t, "engineer", false)
	ctx := context.Background()
	failedID := f.insertWorkerTask(t, "running", "comment", 1, 1)
	fx.Exec(t, "UPDATE agent_task_queue SET issue_id=$2 WHERE id=$1", failedID, f.issueID)
	for n := 0; n < 2; n++ {
		failed, err := svc.FailTask(ctx, failedID, "provider returned HTTP 429", "repair-session", "/synthetic/preserved/worktree", "agent/synthetic/repair", taskfailure.ReasonAgentProviderCapacityOrRateLimit.String(), false, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if failed.Status != "failed" || failed.BranchName.String != "agent/synthetic/repair" || failed.WorkDir.String != "/synthetic/preserved/worktree" {
			t.Fatal("failure callback lost its preserved-work metadata")
		}
	}
	var runs, used, notifications int
	fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", failedID).Scan(&runs)
	fx.QueryRow(t, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used)
	fx.QueryRow(t, "SELECT count(*) FROM inbox_item WHERE workspace_id=$1 AND recipient_id=$2 AND severity='action_required' AND details->>'failed_task_id'=$3", f.workspaceID, f.userID, util.UUIDToString(failedID)).Scan(&notifications)
	if runs != 0 || used != 1 || notifications != 1 {
		t.Fatalf("failed repair: observer_runs=%d charged=%d human_notices=%d", runs, used, notifications)
	}
	var status, assignee string
	fx.QueryRow(t, "SELECT status,assignee_id::text FROM issue WHERE id=$1", f.issueID).Scan(&status, &assignee)
	if status != "in_progress" || assignee != f.worker {
		t.Fatal("failure recovery moved the issue or transferred writer authority")
	}
}

func TestDelegatedFailureDefaultWithoutHandoffsRemainsUnchanged(t *testing.T) {
	f, svc, fx, project := seedWorkflowRecovery(t, "engineer", true)
	failedID := f.insertWorkerTask(t, "running", "comment", 1, 1)
	if _, err := svc.FailTask(context.Background(), failedID, "worker exited", "", "", "", "agent_error.process_failure", false, "", ""); err != nil {
		t.Fatal(err)
	}
	var runs, used int
	fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", failedID).Scan(&runs)
	fx.QueryRow(t, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used)
	if runs != 1 || used != 2 {
		t.Fatalf("Default without handoffs changed legacy behavior: runs=%d used=%d", runs, used)
	}
}

func TestDelegatedFailureWorkflowCoveredRecoveryDoesNotEscalateAfterOwnerChange(t *testing.T) {
	for _, state := range []string{"queued", "completed_legacy_receipt"} {
		t.Run(state, func(t *testing.T) {
			f, svc, fx, project := seedWorkflowRecovery(t, "coordinator", false)
			ctx := context.Background()
			recoveryID, signalID := f.seedRecoverySignal(t, svc)
			if state == "completed_legacy_receipt" {
				f.markDelivered(t, recoveryID, signalID)
				if _, err := svc.CompleteTask(ctx, recoveryID, []byte(`{"output":"Recovery delivered"}`), "", "", "", false, "", ""); err != nil {
					t.Fatal(err)
				}
				// Historical delivered receipts remain authoritative without the newer marker.
				fx.Exec(t, "UPDATE comment SET recovery_settled_at=NULL WHERE id=$1", signalID)
			}
			fx.Exec(t, "UPDATE issue SET assignee_type='member',assignee_id=$2 WHERE id=$1", f.issueID, f.userID)
			var failedID pgtype.UUID
			fx.QueryRow(t, "SELECT source_task_id FROM comment WHERE id=$1", signalID).Scan(&failedID)
			failed, err := svc.Queries.GetAgentTask(ctx, failedID)
			if err != nil {
				t.Fatal(err)
			}
			if handled, err := svc.recoverDelegatedTaskFailure(ctx, failed); err != nil || !handled {
				t.Fatalf("covered replay: %v %v", handled, err)
			}
			var notices, runs, used int
			fx.QueryRow(t, "SELECT count(*) FROM inbox_item WHERE workspace_id=$1 AND details->>'outcome'='needs_human'", f.workspaceID).Scan(&notices)
			fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", failedID).Scan(&runs)
			fx.QueryRow(t, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used)
			if notices != 0 || runs != 1 || used != 2 {
				t.Fatalf("covered recovery changed: notices=%d runs=%d used=%d", notices, runs, used)
			}
			var status string
			fx.QueryRow(t, "SELECT status FROM agent_task_queue WHERE id=$1", recoveryID).Scan(&status)
			if state == "queued" && status != "queued" {
				t.Fatal("already covered queued recovery was cancelled")
			}
		})
	}
}

func TestDelegatedFailureObserverNotificationRespectsTenantMembership(t *testing.T) {
	f, svc, fx, _ := seedWorkflowRecovery(t, "engineer", false)
	foreign, _ := seedDelegatedFailureFixture(t)
	failedID := f.insertWorkerTask(t, "failed", "comment", 1, 1)
	fx.Exec(t, "UPDATE agent_task_queue SET originator_user_id=$2,accountable_user_id=$2 WHERE id=$1", failedID, foreign.userID)
	failed, err := svc.Queries.GetAgentTask(context.Background(), failedID)
	if err != nil {
		t.Fatal(err)
	}
	if handled, err := svc.recoverDelegatedTaskFailure(context.Background(), failed); err != nil || !handled {
		t.Fatalf("human escalation: %v %v", handled, err)
	}
	var leaked int
	var message string
	fx.QueryRow(t, "SELECT count(*) FROM inbox_item WHERE details->>'failed_task_id'=$1", util.UUIDToString(failedID)).Scan(&leaked)
	fx.QueryRow(t, "SELECT content FROM comment WHERE workspace_id=$1 AND issue_id=$2 AND type='system' AND source_task_id=$3", f.workspaceID, f.issueID, failedID).Scan(&message)
	if leaked != 0 || !strings.Contains(message, "needs human attention") {
		t.Fatalf("foreign recipient received data or local explanation missing: inbox=%d message=%s", leaked, message)
	}
}

// Pause the real INSERT after Scan, while its transaction is still open.
// A pool-level INSERT outside the ownership transaction cannot reach this gate.
type workflowRecoveryTxGate struct {
	pool                                               *pgxpool.Pool
	beforeLock, beforeSquadLock, admitted, allowCommit chan struct{}
	lockOnce, squadLockOnce, admitOnce, releaseOnce    sync.Once
}

func (g *workflowRecoveryTxGate) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &workflowRecoveryGatedTx{Tx: tx, gate: g}, nil
}

func (g *workflowRecoveryTxGate) release() {
	if g.allowCommit != nil {
		g.releaseOnce.Do(func() { close(g.allowCommit) })
	}
}

type workflowRecoveryGatedTx struct {
	pgx.Tx
	gate *workflowRecoveryTxGate
}

func (tx *workflowRecoveryGatedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(sql, "-- name: LockSquadForAutopilotAssignment ") && tx.gate.beforeSquadLock != nil {
		tx.gate.squadLockOnce.Do(func() { close(tx.gate.beforeSquadLock) })
	}
	if strings.HasPrefix(sql, "-- name: LockIssueForDescriptionUpdate ") && tx.gate.beforeLock != nil {
		tx.gate.lockOnce.Do(func() { close(tx.gate.beforeLock) })
	}
	row := tx.Tx.QueryRow(ctx, sql, args...)
	if strings.HasPrefix(sql, "-- name: CreateAgentTask ") && tx.gate.admitted != nil {
		return workflowRecoveryGatedRow{Row: row, ctx: ctx, gate: tx.gate}
	}
	return row
}

type workflowRecoveryGatedRow struct {
	pgx.Row
	ctx  context.Context
	gate *workflowRecoveryTxGate
}

func (r workflowRecoveryGatedRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	r.gate.admitOnce.Do(func() { close(r.gate.admitted) })
	select {
	case <-r.gate.allowCommit:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func waitWorkflowRecoveryGate(t *testing.T, ctx context.Context, gate <-chan struct{}) {
	t.Helper()
	select {
	case <-gate:
	case <-ctx.Done():
		t.Fatal("recovery did not reach transaction gate:", ctx.Err())
	}
}

func TestDelegatedFailureWorkflowOwnerUpdateWaitsForAdmissionCommit(t *testing.T) {
	f, svc, fx, project := seedWorkflowRecovery(t, "coordinator", false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	failedID := f.insertWorkerTask(t, "failed", "comment", 1, 1)
	if target, _, err := svc.ensureDelegatedFailureRecoveryComment(ctx, failedID); err != nil || target == nil {
		t.Fatal("seed signal:", err)
	}
	failed, err := svc.Queries.GetAgentTask(ctx, failedID)
	if err != nil {
		t.Fatal(err)
	}
	gate := &workflowRecoveryTxGate{pool: f.pool, admitted: make(chan struct{}), allowCommit: make(chan struct{})}
	svc.TxStarter = gate
	defer gate.release()
	dispatched := make(chan error, 1)
	go func() { _, err := svc.recoverDelegatedTaskFailure(ctx, failed); dispatched <- err }()
	waitWorkflowRecoveryGate(t, ctx, gate.admitted)
	ownerConn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerConn.Release()
	var pid int
	if err := ownerConn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	updated := make(chan error, 1)
	go func() {
		_, err := ownerConn.Exec(ctx, "UPDATE issue SET assignee_type='member',assignee_id=$2 WHERE id=$1", f.issueID, f.userID)
		updated <- err
	}()
	for {
		var waiting bool
		if err := f.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-updated:
			t.Fatalf("owner changed before admission commit: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	gate.release()
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	var runs, used int
	fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", failedID).Scan(&runs)
	fx.QueryRow(t, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used)
	if runs != 1 || used != 2 {
		t.Fatalf("legal admission lost: runs=%d used=%d", runs, used)
	}
}

func TestDelegatedFailureWorkflowRequiresAtomicTransaction(t *testing.T) {
	svc := &TaskService{}
	called := false
	_, err := svc.withDelegatedFailureRecoveryOwnership(context.Background(), &delegatedFailureRecoveryTarget{}, pgtype.UUID{}, func(*db.Queries, *delegatedFailureRecoveryTarget) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("missing transaction admitted work: called=%v err=%v", called, err)
	}
}

func TestDelegatedFailureWorkflowOwnerChangesBeforeAdmissionLock(t *testing.T) {
	for _, status := range []string{"in_progress", "done"} {
		t.Run(status, func(t *testing.T) {
			f, svc, fx, project := seedWorkflowRecovery(t, "coordinator", false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			failedID := f.insertWorkerTask(t, "failed", "comment", 1, 1)
			target, _, err := svc.ensureDelegatedFailureRecoveryComment(ctx, failedID)
			if err != nil || target == nil {
				t.Fatal("seed signal:", err)
			}
			failed, err := svc.Queries.GetAgentTask(ctx, failedID)
			if err != nil {
				t.Fatal(err)
			}
			ownerTx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer ownerTx.Rollback(context.Background())
			if _, err := ownerTx.Exec(ctx, "UPDATE issue SET assignee_type='member',assignee_id=$2,status=$3 WHERE id=$1", f.issueID, f.userID, status); err != nil {
				t.Fatal(err)
			}
			gate := &workflowRecoveryTxGate{pool: f.pool, beforeLock: make(chan struct{})}
			svc.TxStarter = gate
			dispatched := make(chan error, 1)
			go func() { _, err := svc.recoverDelegatedTaskFailure(ctx, failed); dispatched <- err }()
			waitWorkflowRecoveryGate(t, ctx, gate.beforeLock)
			if err := ownerTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-dispatched; err != nil {
				t.Fatal(err)
			}
			var runs, used, notices int
			fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", failedID).Scan(&runs)
			fx.QueryRow(t, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used)
			fx.QueryRow(t, "SELECT count(*) FROM inbox_item WHERE workspace_id=$1 AND details->>'outcome'='needs_human'", f.workspaceID).Scan(&notices)
			wantNotices := 1
			if status == "done" {
				wantNotices = 0
			}
			if runs != 0 || used != 1 || notices != wantNotices {
				t.Fatalf("stale ownership/lifecycle used: runs=%d used=%d inbox=%d", runs, used, notices)
			}
			if f.settled(t, target.comment.ID) != (status == "in_progress") {
				t.Fatal("terminal issue incorrectly settled its pending signal")
			}
		})
	}
}

func TestDelegatedFailureWorkflowSquadLeaderRotationSerializesWithAdmission(t *testing.T) {
	for _, rotationFirst := range []bool{false, true} {
		name := "admission_first"
		if rotationFirst {
			name = "rotation_first"
		}
		t.Run(name, func(t *testing.T) {
			f, svc, fx, project := seedWorkflowRecovery(t, "squad", false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			failedID := f.insertWorkerTask(t, "failed", "comment", 1, 1)
			if target, _, err := svc.ensureDelegatedFailureRecoveryComment(ctx, failedID); err != nil || target == nil {
				t.Fatal("seed signal:", err)
			}
			failed, err := svc.Queries.GetAgentTask(ctx, failedID)
			if err != nil {
				t.Fatal(err)
			}
			var squad pgtype.UUID
			fx.QueryRow(t, "SELECT squad_id FROM agent_task_queue WHERE id=$1", f.sourceTask).Scan(&squad)
			gate := &workflowRecoveryTxGate{pool: f.pool, admitted: make(chan struct{}), allowCommit: make(chan struct{}), beforeSquadLock: make(chan struct{})}
			svc.TxStarter = gate
			defer gate.release()
			rotationTx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rotationTx.Rollback(context.Background())
			var pid int
			if err := rotationTx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			rotate := func() error {
				qtx := svc.Queries.WithTx(rotationTx)
				if _, err := qtx.LockSquadForUpdate(ctx, db.LockSquadForUpdateParams{ID: squad, WorkspaceID: util.MustParseUUID(f.workspaceID)}); err != nil {
					return err
				}
				if _, err := qtx.LockAgentForAutopilotAssignment(ctx, db.LockAgentForAutopilotAssignmentParams{ID: util.MustParseUUID(f.worker), WorkspaceID: util.MustParseUUID(f.workspaceID)}); err != nil {
					return err
				}
				_, err := rotationTx.Exec(ctx, "UPDATE squad SET leader_id=$2 WHERE id=$1", squad, f.worker)
				return err
			}
			if rotationFirst {
				if err := rotate(); err != nil {
					t.Fatal(err)
				}
			}
			dispatched := make(chan error, 1)
			go func() { _, err := svc.recoverDelegatedTaskFailure(ctx, failed); dispatched <- err }()
			if rotationFirst {
				waitWorkflowRecoveryGate(t, ctx, gate.beforeSquadLock)
				if err := rotationTx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				waitWorkflowRecoveryGate(t, ctx, gate.admitted)
				rotated := make(chan error, 1)
				go func() { rotated <- rotate() }()
				for {
					var waiting bool
					if err := f.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')", pid).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case err := <-rotated:
						t.Fatalf("squad leader rotated before admission commit: %v", err)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(5 * time.Millisecond):
					}
				}
				gate.release()
				if err := <-rotated; err != nil {
					t.Fatal(err)
				}
				if err := rotationTx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-dispatched; err != nil {
				t.Fatal(err)
			}
			var runs, used, notices int
			fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", failedID).Scan(&runs)
			fx.QueryRow(t, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", project).Scan(&used)
			fx.QueryRow(t, "SELECT count(*) FROM inbox_item WHERE workspace_id=$1 AND details->>'outcome'='needs_human'", f.workspaceID).Scan(&notices)
			if rotationFirst {
				if runs != 0 || used != 1 || notices != 1 {
					t.Fatalf("stale squad leader admitted: runs=%d used=%d inbox=%d", runs, used, notices)
				}
			} else if runs != 1 || used != 2 || notices != 0 {
				t.Fatalf("legal squad admission lost: runs=%d used=%d inbox=%d", runs, used, notices)
			}
		})
	}
}
