-- name: GetWorkspaceRunBudget :one
SELECT * FROM workspace_run_budget WHERE workspace_id=$1;

-- name: LockWorkspaceForRunBudget :one
SELECT id FROM workspace WHERE id=$1 FOR UPDATE;

-- name: WorkspaceHasUnfinishedRuns :one
SELECT EXISTS(SELECT 1 FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id
WHERE a.workspace_id=$1 AND t.status NOT IN ('completed','failed','cancelled'));

-- name: ConfigureWorkspaceRunBudget :one
INSERT INTO workspace_run_budget(workspace_id,max_runs,deadline,paused)
VALUES (@workspace_id, @max_runs, @deadline, @paused)
ON CONFLICT (workspace_id) DO NOTHING
RETURNING *;

-- name: UpdateWorkspaceRunBudget :one
UPDATE workspace_run_budget SET max_runs= @max_runs,deadline= @deadline,paused= @paused,
 revision=revision+1,updated_at=clock_timestamp(),last_refusal=NULL
WHERE workspace_id= @workspace_id AND revision= @expected_revision
 AND @max_runs >= admitted_runs
RETURNING *;
