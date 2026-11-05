CREATE FUNCTION direct_task_budget_projects(p_issue uuid,p_chat uuid,p_autopilot_run uuid,p_context jsonb,p_workspace uuid)
RETURNS uuid[] LANGUAGE sql AS $$
    SELECT coalesce(array_agg(p.id ORDER BY p.id),'{}'::uuid[])
    FROM project p WHERE p.workspace_id=p_workspace AND (
        p.id=(SELECT project_id FROM issue WHERE id=p_issue)
        OR p.id=(SELECT project_id FROM chat_session WHERE id=p_chat)
        OR p.id=(SELECT a.project_id FROM autopilot a JOIN autopilot_run r ON r.autopilot_id=a.id WHERE r.id=p_autopilot_run)
        OR p.id::text=p_context->>'project_id'
        OR p.id=(SELECT project_id FROM issue WHERE workspace_id=p_workspace AND id::text=p_context->>'parent_issue_id')
    );
$$;

CREATE FUNCTION resolve_task_budget_projects(p_task uuid)
RETURNS uuid[] LANGUAGE sql AS $$
    SELECT coalesce((SELECT coalesce(t.budget_project_ids,
        direct_task_budget_projects(t.issue_id,t.chat_session_id,t.autopilot_run_id,t.context,a.workspace_id))
        FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id WHERE t.id=p_task),'{}'::uuid[]);
$$;

CREATE FUNCTION admit_project_run() RETURNS trigger LANGUAGE plpgsql AS $$
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
        reason := CASE WHEN b.paused THEN 'paused'
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

CREATE FUNCTION count_admitted_project_run() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b uuid;
BEGIN
    FOR b IN SELECT project_id FROM project_run_budget
        WHERE project_id=ANY(NEW.budget_project_ids) ORDER BY project_id FOR UPDATE LOOP
        UPDATE project_run_budget SET admitted_runs=admitted_runs+1,updated_at=clock_timestamp()
        WHERE project_id=b AND admitted_runs<max_runs;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'project run budget exceeded by batch' USING ERRCODE='54000';
        END IF;
    END LOOP;
    RETURN NEW;
END;
$$;

CREATE FUNCTION preserve_task_budget_projects() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.budget_project_ids IS DISTINCT FROM OLD.budget_project_ids THEN
        RAISE EXCEPTION 'task budget project snapshot is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER zz_admit_project_run BEFORE INSERT ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION admit_project_run();
CREATE TRIGGER zz_count_admitted_project_run AFTER INSERT ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION count_admitted_project_run();
CREATE TRIGGER zz_preserve_task_budget_projects BEFORE UPDATE OF budget_project_ids ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION preserve_task_budget_projects();
