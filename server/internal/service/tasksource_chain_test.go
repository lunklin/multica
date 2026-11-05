package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/tasksource"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestAuthenticatedTaskSourcePropagatesAcrossNativeEnqueues(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	base := context.Background()
	ws, user, agentID, _ := seedAttributionFixture(t, pool)
	f := testutil.New(pool, ws, user)
	f.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", agentID)
	q := db.New(pool)
	agent, err := q.GetAgent(base, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	sourceProject := f.Project(t, "synthetic bounded caller")
	sourceIssue := f.Issue(t, "caller issue", testutil.Cols{"project_id": sourceProject})
	if _, err := pool.Exec(base, "INSERT INTO project_run_budget(project_id,workspace_id,max_runs,deadline) VALUES($1,$2,9,clock_timestamp()+interval '1 hour')", sourceProject, ws); err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM project_run_budget WHERE project_id=$1", sourceProject)
	sourceID := f.Task(t, agentID, testutil.Cols{"runtime_id": agent.RuntimeID, "issue_id": sourceIssue, "status": "completed"})
	source := util.MustParseUUID(sourceID)
	ctx := tasksource.WithTaskID(base, source)
	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	newIssue := func(t *testing.T) db.Issue {
		t.Helper()
		id := f.Issue(t, "source task target", testutil.Cols{"assignee_type": "agent", "assignee_id": agentID})
		issue, err := q.GetIssue(base, util.MustParseUUID(id))
		if err != nil {
			t.Fatal(err)
		}
		return issue
	}
	newChat := func(t *testing.T) db.ChatSession {
		t.Helper()
		id := f.ChatSession(t, agentID)
		chat, err := q.GetChatSession(base, util.MustParseUUID(id))
		if err != nil {
			t.Fatal(err)
		}
		return chat
	}
	admitted := 1
	var lastChild pgtype.UUID
	assertSource := func(t *testing.T, task db.AgentTaskQueue) {
		t.Helper()
		stored, err := q.GetAgentTask(base, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.DelegatedFromTaskID != source {
			t.Fatalf("stored task lost authenticated caller: got %s want %s", util.UUIDToString(stored.DelegatedFromTaskID), sourceID)
		}
		if stored.OriginatorUserID != util.MustParseUUID(user) || stored.AccountableUserID != util.MustParseUUID(user) {
			t.Fatalf("source propagation changed human attribution: %+v", stored)
		}
		found := false
		for _, scope := range stored.BudgetProjectIds {
			if scope == util.MustParseUUID(sourceProject) {
				found = true
			}
		}
		if !found {
			t.Fatalf("stored task lost caller project scope: %+v", stored.BudgetProjectIds)
		}
		admitted++
		var used int
		if err := pool.QueryRow(base, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", sourceProject).Scan(&used); err != nil || used != admitted {
			t.Fatalf("project admitted runs=%d want %d, err=%v", used, admitted, err)
		}
		lastChild = task.ID
	}
	for _, tc := range []struct {
		name    string
		enqueue func(*testing.T) (db.AgentTaskQueue, error)
	}{
		{"existing issue assignment", func(t *testing.T) (db.AgentTaskQueue, error) { return svc.EnqueueTaskForIssue(ctx, newIssue(t)) }},
		{"workflow handoff", func(t *testing.T) (db.AgentTaskQueue, error) {
			return svc.EnqueueTaskForIssueWithHandoff(ctx, newIssue(t), "one handoff", util.MustParseUUID(user))
		}},
		{"explicit mention", func(t *testing.T) (db.AgentTaskQueue, error) {
			return svc.EnqueueTaskForMention(ctx, newIssue(t), agent.ID, pgtype.UUID{}, OriginNamed)
		}},
		{"deferred channel issue", func(t *testing.T) (db.AgentTaskQueue, error) {
			return svc.EnqueueDeferredChannelIssueTask(ctx, newIssue(t), time.Now().Add(time.Minute))
		}},
		{"quick create without project", func(t *testing.T) (db.AgentTaskQueue, error) {
			return svc.EnqueueQuickCreateTask(ctx, util.MustParseUUID(ws), util.MustParseUUID(user), agent.ID, pgtype.UUID{}, "synthetic task", "medium", "", pgtype.UUID{}, pgtype.UUID{}, nil)
		}},
		{"chat enqueue without project", func(t *testing.T) (db.AgentTaskQueue, error) {
			return svc.EnqueueChatTask(ctx, newChat(t), util.MustParseUUID(user), false)
		}},
		{"direct chat send without project", func(t *testing.T) (db.AgentTaskQueue, error) {
			sent, err := svc.SendDirectChatMessage(ctx, newChat(t), agent, util.MustParseUUID(user), "synthetic delegation", nil, "member", util.MustParseUUID(user))
			if err != nil {
				return db.AgentTaskQueue{}, err
			}
			return sent.Task, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, err := tc.enqueue(t)
			if err != nil {
				t.Fatal(err)
			}
			assertSource(t, task)
		})
	}
	t.Run("autopilot direct run", func(t *testing.T) {
		apID, runID := seedRunOnlyAutopilot(t, pool, ws, agentID, user)
		ap, err := q.GetAutopilot(ctx, util.MustParseUUID(apID))
		if err != nil {
			t.Fatal(err)
		}
		run, err := q.GetAutopilotRun(ctx, util.MustParseUUID(runID))
		if err != nil {
			t.Fatal(err)
		}
		auto := &AutopilotService{Queries: q, TxStarter: pool, Bus: events.New(), TaskSvc: svc}
		if err := auto.dispatchRunOnly(ctx, ap, &run, util.MustParseUUID(user)); err != nil {
			t.Fatal(err)
		}
		task, err := q.GetAutopilotTaskByRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertSource(t, task)
	})
	t.Run("unscoped descendant is refused after all eight native admissions", func(t *testing.T) {
		var id string
		err := pool.QueryRow(base, `INSERT INTO agent_task_queue(agent_id,runtime_id,delegated_from_task_id,status)
 SELECT id,runtime_id,$2,'completed' FROM agent WHERE id=$1 RETURNING id`, agentID, lastChild).Scan(&id)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("exhausted caller project admitted descendant %q: %v", id, err)
		}
		var used int
		if err := pool.QueryRow(base, "SELECT admitted_runs FROM project_run_budget WHERE project_id=$1", sourceProject).Scan(&used); err != nil || used != 9 {
			t.Fatalf("refusal changed admitted runs=%d, err=%v", used, err)
		}
	})
}
