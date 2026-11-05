-- name: GetProjectRunBudget :one
SELECT * FROM project_run_budget
WHERE workspace_id = @workspace_id::uuid AND project_id = @project_id::uuid;

-- name: LockProjectForRunBudget :one
SELECT id FROM project
WHERE workspace_id = @workspace_id::uuid AND id = @project_id::uuid
FOR UPDATE;

-- name: ProjectHasUnfinishedBudgetRuns :one
SELECT EXISTS (
    SELECT 1
    FROM agent_task_queue t
    JOIN agent a ON a.id = t.agent_id
    WHERE a.workspace_id = @workspace_id::uuid
      AND t.status NOT IN ('completed', 'failed', 'cancelled')
      AND @project_id::uuid = ANY(resolve_task_budget_projects(t.id)::uuid[])
);

-- name: ConfigureProjectRunBudget :one
INSERT INTO project_run_budget(workspace_id, project_id, max_runs, deadline, paused, stop_on_failure)
VALUES (@workspace_id::uuid, @project_id::uuid, @max_runs, @deadline, @paused, COALESCE(sqlc.narg(stop_on_failure)::boolean,false))
ON CONFLICT (project_id) DO NOTHING
RETURNING *;

-- name: IsTaskProjectRunBudgeted :one
SELECT EXISTS (
    SELECT 1 FROM project_run_budget b
    WHERE b.workspace_id = @workspace_id::uuid
      AND b.project_id = ANY(resolve_task_budget_projects(@task_id::uuid)::uuid[])
);

-- name: UpdateProjectRunBudget :one
UPDATE project_run_budget
SET max_runs = @max_runs, deadline = @deadline,
    paused = CASE WHEN failure_task_id IS NOT NULL AND NOT @clear_failure_latch::boolean THEN true ELSE @paused END,
    failure_task_id = CASE WHEN @clear_failure_latch::boolean AND NOT @paused::boolean THEN NULL ELSE failure_task_id END,
    stop_on_failure = COALESCE(sqlc.narg(stop_on_failure)::boolean,stop_on_failure),
    revision = revision + 1, updated_at = clock_timestamp(), last_refusal = NULL
WHERE workspace_id = @workspace_id::uuid AND project_id = @project_id::uuid
  AND revision = @expected_revision
  AND @max_runs >= admitted_runs
RETURNING *;

-- name: IsTaskProjectFailureBlocked :one
SELECT task_project_failure_blocked(@task_id::uuid)::boolean AS blocked;

-- name: LockTaskProjectBudgets :one
SELECT lock_task_project_budgets(@task_ids::uuid[])::bigint AS locked;
