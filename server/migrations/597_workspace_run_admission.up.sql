-- Use one locked budget row for every queue insertion path, including chat,
-- workflow, wakeup, autopilot and retry. A refused retry must not roll back
-- its parent's failed status, hence the BEFORE trigger's no-row contract.
CREATE FUNCTION admit_workspace_run() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    b workspace_run_budget%ROWTYPE;
    reason text;
BEGIN
    SELECT * INTO b FROM workspace_run_budget
    WHERE workspace_id=(SELECT workspace_id FROM agent WHERE id=NEW.agent_id)
    FOR UPDATE;
    IF NOT FOUND THEN RETURN NEW; END IF;
    reason := CASE WHEN b.paused THEN 'paused'
                   WHEN b.deadline<=clock_timestamp() THEN 'deadline'
                   WHEN b.admitted_runs>=b.max_runs THEN 'run_limit' END;
    IF reason IS NOT NULL THEN
        UPDATE workspace_run_budget SET last_refusal=reason,updated_at=clock_timestamp()
        WHERE workspace_id=b.workspace_id;
        RETURN NULL;
    END IF;
    NEW.context := COALESCE(NEW.context,'{}'::jsonb) ||
        jsonb_build_object('workspace_run_budget',b.workspace_id::text);
    RETURN NEW;
END;
$$;

CREATE FUNCTION count_admitted_workspace_run() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- AFTER INSERT does not run for ON CONFLICT DO NOTHING. The counter is
    -- never refunded by cancellation, failure, task deletion or re-claim.
    UPDATE workspace_run_budget SET admitted_runs=admitted_runs+1,updated_at=clock_timestamp()
    WHERE workspace_id=(SELECT workspace_id FROM agent WHERE id=NEW.agent_id)
      AND admitted_runs<max_runs;
    IF NOT FOUND AND EXISTS (
        SELECT 1 FROM workspace_run_budget
        WHERE workspace_id=(SELECT workspace_id FROM agent WHERE id=NEW.agent_id)
    ) THEN
        -- Row-level BEFORE triggers precede the AFTER triggers in a bulk
        -- statement. Refuse the whole batch rather than admit excess work.
        RAISE EXCEPTION 'workspace run budget exceeded by batch' USING ERRCODE='54000';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_admit_workspace_run BEFORE INSERT ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION admit_workspace_run();
CREATE TRIGGER zz_count_admitted_workspace_run AFTER INSERT ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION count_admitted_workspace_run();
