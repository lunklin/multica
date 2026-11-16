-- Candidate only: never apply as part of a CLI operation.
-- Object locations are server-private recovery data, not public receipts.
CREATE TABLE IF NOT EXISTS attachment_admin_record (
    kind text NOT NULL CHECK (kind IN ('plan', 'operation')),
    id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    actor_id uuid NOT NULL,
    data jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
