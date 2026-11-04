CREATE TABLE project_run_budget (
    project_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    max_runs bigint NOT NULL CHECK (max_runs > 0),
    admitted_runs bigint NOT NULL DEFAULT 0 CHECK (admitted_runs >= 0),
    deadline timestamptz NOT NULL,
    paused boolean NOT NULL DEFAULT false,
    revision bigint NOT NULL DEFAULT 1,
    last_refusal text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
-- NULL identifies pre-migration history; every new admission gets a snapshot,
-- including an empty array for a task with no project scope.
ALTER TABLE agent_task_queue ADD COLUMN budget_project_ids uuid[];
