-- An opt-in workspace ceiling counts new queue rows, not model requests.
-- No budget row means the existing admission behavior is unchanged.
CREATE TABLE workspace_run_budget (
    workspace_id uuid NOT NULL,
    max_runs bigint NOT NULL CHECK (max_runs > 0),
    admitted_runs bigint NOT NULL DEFAULT 0 CHECK (admitted_runs >= 0),
    deadline timestamptz NOT NULL,
    paused boolean NOT NULL DEFAULT false,
    revision bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_refusal text
);
