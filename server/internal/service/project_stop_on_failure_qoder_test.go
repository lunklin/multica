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
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Hold one real lifecycle transaction after its Task CAS, before syncQoderIssue.
type projectFailureLifecycleGate struct {
	pool             *pgxpool.Pool
	query            string
	reached, release chan struct{}
	once             sync.Once
}

func (g *projectFailureLifecycleGate) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &projectFailureLifecycleTx{Tx: tx, gate: g}, nil
}

type projectFailureLifecycleTx struct {
	pgx.Tx
	gate *projectFailureLifecycleGate
}

func (tx *projectFailureLifecycleTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, sql, args...)
	if strings.HasPrefix(sql, "-- name: "+tx.gate.query+" ") {
		return projectFailureLifecycleRow{Row: row, ctx: ctx, gate: tx.gate}
	}
	return row
}

type projectFailureLifecycleRow struct {
	pgx.Row
	ctx  context.Context
	gate *projectFailureLifecycleGate
}

func (r projectFailureLifecycleRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	r.gate.once.Do(func() { close(r.gate.reached) })
	select {
	case <-r.gate.release:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func TestProjectStopOnFailureQoderStartSerializesWithRecovery(t *testing.T) {
	for _, method := range []string{"legacy", "claim"} {
		for _, first := range []string{"recovery", "start"} {
			t.Run(method+"/"+first, func(t *testing.T) {
				f, svc, fx, _ := seedStopOnFailure(t)
				fx.Exec(t, "UPDATE agent_runtime SET provider='qoder_cloud' WHERE id=$1", f.runtimeID)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				id := util.MustParseUUID(fx.Task(t, f.coordinator, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.issueID, "status": "dispatched", "dispatched_at": time.Now()}))
				task, err := svc.Queries.GetAgentTask(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				start := func(s *TaskService) error {
					if method == "legacy" {
						_, err := s.StartTask(ctx, id)
						return err
					}
					_, err := s.StartTaskForClaim(ctx, db.LockAgentTaskStartClaimParams{ID: id, RuntimeID: task.RuntimeID, DispatchedAt: task.DispatchedAt})
					return err
				}
				recoveryConn, err := f.pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer recoveryConn.Release()
				recoveryTx, err := recoveryConn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer recoveryTx.Rollback(context.Background())
				var recoveryPID int
				if err := recoveryConn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&recoveryPID); err != nil {
					t.Fatal(err)
				}
				recovery := func() error {
					q := svc.Queries.WithTx(recoveryTx)
					if _, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: util.MustParseUUID(f.issueID), WorkspaceID: util.MustParseUUID(f.workspaceID)}); err != nil {
						return err
					}
					_, err := q.CreateAgentTask(ctx, db.CreateAgentTaskParams{AgentID: util.MustParseUUID(f.worker), RuntimeID: util.MustParseUUID(f.runtimeID), IssueID: util.MustParseUUID(f.issueID)})
					if err != nil {
						return err
					}
					return recoveryTx.Commit(ctx)
				}
				if first == "recovery" {
					if _, err := svc.Queries.WithTx(recoveryTx).LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: util.MustParseUUID(f.issueID), WorkspaceID: util.MustParseUUID(f.workspaceID)}); err != nil {
						t.Fatal(err)
					}
					startConn, err := f.pool.Acquire(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer startConn.Release()
					var pid int
					if err := startConn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
						t.Fatal(err)
					}
					startSvc := &TaskService{Queries: db.New(startConn), TxStarter: startConn, Bus: svc.Bus}
					started := make(chan error, 1)
					go func() { started <- start(startSvc) }()
					waitProjectFailureLock(t, ctx, f, pid, started)
					// Start waiting for the issue must not already own its budget.
					if _, err := recoveryTx.Exec(ctx, "SELECT project_id FROM project_run_budget WHERE project_id=ANY(resolve_task_budget_projects($1)) FOR UPDATE NOWAIT", id); err != nil {
						t.Fatal("Qoder start reversed issue/budget order:", err)
					}
					if err := recovery(); err != nil {
						t.Fatal(err)
					}
					if err := <-started; err != nil {
						t.Fatal(err)
					}
				} else {
					gate := &projectFailureLifecycleGate{pool: f.pool, query: "StartAgentTaskWithSupplement", reached: make(chan struct{}), release: make(chan struct{})}
					startSvc := &TaskService{Queries: svc.Queries, TxStarter: gate, Bus: svc.Bus}
					defer func() {
						select {
						case <-gate.release:
						default:
							close(gate.release)
						}
					}()
					started := make(chan error, 1)
					go func() { started <- start(startSvc) }()
					waitWorkflowRecoveryGate(t, ctx, gate.reached)
					recovered := make(chan error, 1)
					go func() { recovered <- recovery() }()
					waitProjectFailureLock(t, ctx, f, recoveryPID, recovered)
					close(gate.release)
					if err := <-started; err != nil {
						t.Fatal(err)
					}
					if err := <-recovered; err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestProjectStopOnFailureQoderCompleteAndStartSerialize(t *testing.T) {
	for _, first := range []string{"complete", "start"} {
		t.Run(first, func(t *testing.T) {
			f, svc, fx, _ := seedStopOnFailure(t)
			fx.Exec(t, "UPDATE agent_runtime SET provider='qoder_cloud' WHERE id=$1", f.runtimeID)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			active := util.MustParseUUID(fx.Task(t, f.worker, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "running"}))
			prepared := util.MustParseUUID(fx.Task(t, f.worker, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "dispatched", "dispatched_at": time.Now()}))
			query := "CompleteAgentTask"
			if first == "start" {
				query = "StartAgentTaskWithSupplement"
			}
			gate := &projectFailureLifecycleGate{pool: f.pool, query: query, reached: make(chan struct{}), release: make(chan struct{})}
			defer func() {
				select {
				case <-gate.release:
				default:
					close(gate.release)
				}
			}()
			gated := &TaskService{Queries: svc.Queries, TxStarter: gate, Bus: svc.Bus}
			complete := func(s *TaskService) error {
				_, err := s.CompleteTask(ctx, active, []byte(`{"ok":true}`), "", "", "", false, "", "")
				return err
			}
			start := func(s *TaskService) error { _, err := s.StartTask(ctx, prepared); return err }
			winner := make(chan error, 1)
			go func() {
				if first == "complete" {
					winner <- complete(gated)
				} else {
					winner <- start(gated)
				}
			}()
			waitWorkflowRecoveryGate(t, ctx, gate.reached)
			conn, err := f.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			var pid int
			if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			secondSvc := &TaskService{Queries: db.New(conn), TxStarter: conn, Bus: svc.Bus}
			second := make(chan error, 1)
			go func() {
				if first == "complete" {
					second <- start(secondSvc)
				} else {
					second <- complete(secondSvc)
				}
			}()
			waitProjectFailureLock(t, ctx, f, pid, second)
			// The loser must wait at the issue, before owning its distinct Task row.
			probe, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Rollback(context.Background())
			loserID := prepared
			if first == "start" {
				loserID = active
			}
			if _, err := probe.Exec(ctx, "SELECT id FROM agent_task_queue WHERE id=$1 FOR UPDATE NOWAIT", loserID); err != nil {
				t.Fatal("Qoder lifecycle locked task before issue:", err)
			}
			if err := probe.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			close(gate.release)
			if err := <-winner; err != nil {
				t.Fatal(err)
			}
			if err := <-second; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Stop exactly after dispatch's optimistic latch read, before its transaction.
type projectFailureBeginGate struct {
	pool             *pgxpool.Pool
	reached, release chan struct{}
	once             sync.Once
}

func (g *projectFailureBeginGate) Begin(ctx context.Context) (pgx.Tx, error) {
	g.once.Do(func() { close(g.reached) })
	select {
	case <-g.release:
		return g.pool.Begin(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestProjectStopOnFailureRecoveryRechecksLatchBeforeMerging(t *testing.T) {
	f, svc, fx, project := seedStopOnFailure(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	independent := fx.Project(t, "independent pending scope")
	fx.Exec(t, "UPDATE issue SET project_id=$2 WHERE id=$1", f.issueID, independent)
	pending := util.MustParseUUID(fx.Task(t, f.coordinator, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.issueID, "status": "queued", "trigger_comment_id": f.sourceTrigger}))
	fx.Exec(t, "UPDATE issue SET project_id=$2 WHERE id=$1", f.issueID, project)
	failedID := f.insertWorkerTask(t, "failed", "comment", 1, 1)
	target, _, err := svc.ensureDelegatedFailureRecoveryComment(ctx, failedID)
	if err != nil || target == nil {
		t.Fatal(err)
	}
	probe, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := svc.Queries.WithTx(probe).MergeDelegatedFailureCommentIntoPendingTask(ctx, db.MergeDelegatedFailureCommentIntoPendingTaskParams{IssueID: target.issue.ID, AgentID: target.agent.ID, CommentID: target.comment.ID})
	if err != nil || merged.ID != pending {
		t.Fatalf("fixture did not expose the native merge path: %v", err)
	}
	if err := probe.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	trigger := util.MustParseUUID(fx.Task(t, f.worker, dbfx.Cols{"runtime_id": f.runtimeID, "issue_id": f.workerIssue, "status": "running"}))
	gate := &projectFailureBeginGate{pool: f.pool, reached: make(chan struct{}), release: make(chan struct{})}
	svc.TxStarter = gate
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	dispatched := make(chan error, 1)
	go func() { _, err := svc.dispatchDelegatedFailureRecovery(ctx, target, pgtype.UUID{}); dispatched <- err }()
	waitWorkflowRecoveryGate(t, ctx, gate.reached)
	failureSvc := &TaskService{Queries: svc.Queries, TxStarter: f.pool, Bus: svc.Bus}
	if _, err := failureSvc.FailTask(ctx, trigger, "another execution failed", "", "", "", "agent_error.process_failure", false, "", ""); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	row, err := svc.Queries.GetAgentTask(ctx, pending)
	if err != nil {
		t.Fatal(err)
	}
	if row.TriggerCommentID != util.MustParseUUID(f.sourceTrigger) || len(row.CoalescedCommentIds) > 0 {
		t.Fatal("stale optimistic read merged the failed-scope signal into independent pending scope")
	}
	if f.settled(t, target.comment.ID) {
		t.Fatal("failure latch consumed the durable recovery signal")
	}
	var runs int
	fx.QueryRow(t, "SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind='delegated_failure' AND trigger_evidence_ref_id=$1", failedID).Scan(&runs)
	if runs != 0 {
		t.Fatal("latched recovery admitted another run")
	}
}
