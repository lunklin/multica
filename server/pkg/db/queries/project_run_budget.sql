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
INSERT INTO project_run_budget(workspace_id, project_id, max_runs, deadline, paused)
VALUES (@workspace_id::uuid, @project_id::uuid, @max_runs, @deadline, @paused)
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
SET max_runs = @max_runs, deadline = @deadline, paused = @paused,
    revision = revision + 1, updated_at = clock_timestamp(), last_refusal = NULL
WHERE workspace_id = @workspace_id::uuid AND project_id = @project_id::uuid
  AND revision = @expected_revision
  AND @max_runs >= admitted_runs
RETURNING *;
