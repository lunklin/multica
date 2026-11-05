-- A failure latch is separate from the irreversible admission counter.
ALTER TABLE project_run_budget
    ADD COLUMN stop_on_failure boolean NOT NULL DEFAULT false,
    ADD COLUMN failure_task_id uuid;

-- Call before taking task-row locks. Policy changes, failure and start all
-- serialize on the same sorted budget rows, including initially opt-out rows.
CREATE FUNCTION lock_task_project_budgets(p_tasks uuid[]) RETURNS bigint
LANGUAGE plpgsql AS $$
DECLARE locked bigint;
BEGIN
    PERFORM w.id FROM workspace w WHERE w.id IN (
        SELECT a.workspace_id FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id WHERE t.id=ANY(p_tasks)
    ) ORDER BY w.id FOR KEY SHARE;
    SELECT count(*) INTO locked FROM (
        SELECT b.project_id FROM project_run_budget b
        WHERE EXISTS (
            SELECT 1 FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id
            WHERE t.id=ANY(p_tasks) AND a.workspace_id=b.workspace_id
              AND b.project_id=ANY(resolve_task_budget_projects(t.id))
        ) ORDER BY b.project_id FOR UPDATE
    ) locked_budgets;
    RETURN locked;
END;
$$;

-- The caller passes only IDs returned by its failed-state CAS, after locking
-- their budgets. First failure wins; replays and subsequent failures retain it.
CREATE FUNCTION latch_failed_project_budgets(p_tasks uuid[]) RETURNS bigint
LANGUAGE plpgsql AS $$
DECLARE changed bigint;
BEGIN
    UPDATE project_run_budget b
    SET paused=true, failure_task_id=(
            SELECT t.id FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id
            WHERE t.id=ANY(p_tasks) AND t.status='failed' AND a.workspace_id=b.workspace_id
              AND b.project_id=ANY(resolve_task_budget_projects(t.id))
            ORDER BY t.id LIMIT 1
        ), revision=revision+1, updated_at=clock_timestamp()
    WHERE b.stop_on_failure AND b.failure_task_id IS NULL
      AND EXISTS (
          SELECT 1 FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id
          WHERE t.id=ANY(p_tasks) AND t.status='failed' AND a.workspace_id=b.workspace_id
            AND b.project_id=ANY(resolve_task_budget_projects(t.id))
      );
    GET DIAGNOSTICS changed=ROW_COUNT;
    RETURN changed;
END;
$$;

CREATE FUNCTION task_project_failure_blocked(p_task uuid) RETURNS boolean
LANGUAGE plpgsql AS $$
BEGIN
    RETURN EXISTS (
        SELECT 1 FROM project_run_budget b
        JOIN agent_task_queue t ON t.id=p_task JOIN agent a ON a.id=t.agent_id
        WHERE b.workspace_id=a.workspace_id AND b.failure_task_id IS NOT NULL
          AND b.project_id=ANY(resolve_task_budget_projects(t.id))
    );
END;
$$;

CREATE OR REPLACE FUNCTION admit_project_run() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    workspace uuid;
    scopes uuid[];
    source uuid;
    b project_run_budget%ROWTYPE;
    reason text;
BEGIN
    SELECT workspace_id INTO workspace FROM agent WHERE id=NEW.agent_id;
    scopes := direct_task_budget_projects(NEW.issue_id,NEW.chat_session_id,NEW.autopilot_run_id,NEW.context,workspace);
    FOR source IN SELECT DISTINCT id FROM unnest(ARRAY[
        NEW.parent_task_id,NEW.retry_of_task_id,NEW.rerun_of_task_id,
        NEW.delegated_from_task_id,NEW.escalation_for_task_id,
        (SELECT origin_id FROM issue WHERE id=NEW.issue_id AND origin_type='agent_create'),
        (SELECT source_task_id FROM issue_wakeup WHERE workspace_id=workspace AND id::text=NEW.context->>'wakeup_id'),
        CASE WHEN NEW.trigger_evidence_kind='delegated_failure' THEN NEW.trigger_evidence_ref_id ELSE NULL::uuid END
    ]) AS ids(id) WHERE id IS NOT NULL LOOP
        IF EXISTS(SELECT 1 FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id WHERE t.id=source AND a.workspace_id=workspace) THEN
            scopes := scopes || resolve_task_budget_projects(source);
        END IF;
    END LOOP;
    -- The caller cannot choose, clear or replace this admission snapshot.
    SELECT coalesce(array_agg(DISTINCT id ORDER BY id),'{}'::uuid[]) INTO NEW.budget_project_ids FROM unnest(scopes) AS ids(id);
    FOR b IN SELECT * FROM project_run_budget
        WHERE workspace_id=workspace AND project_id=ANY(NEW.budget_project_ids)
        ORDER BY project_id FOR UPDATE LOOP
        reason := CASE WHEN b.failure_task_id IS NOT NULL THEN 'failure_latch'
                       WHEN b.paused THEN 'paused'
                       WHEN b.deadline<=clock_timestamp() THEN 'deadline'
                       WHEN b.admitted_runs>=b.max_runs THEN 'run_limit' END;
        IF reason IS NOT NULL THEN
            UPDATE project_run_budget SET last_refusal=reason,updated_at=clock_timestamp() WHERE project_id=b.project_id;
            RETURN NULL;
        END IF;
    END LOOP;
    RETURN NEW;
END;
$$;

