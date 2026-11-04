DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM project_run_budget) THEN
        RAISE EXCEPTION 'retain project run admission while budgets exist';
    END IF;
END $$;
DROP TRIGGER IF EXISTS zz_preserve_task_budget_projects ON agent_task_queue;
DROP TRIGGER IF EXISTS zz_count_admitted_project_run ON agent_task_queue;
DROP TRIGGER IF EXISTS zz_admit_project_run ON agent_task_queue;
DROP FUNCTION IF EXISTS preserve_task_budget_projects();
DROP FUNCTION IF EXISTS count_admitted_project_run();
DROP FUNCTION IF EXISTS admit_project_run();
DROP FUNCTION IF EXISTS resolve_task_budget_projects(uuid);
DROP FUNCTION IF EXISTS direct_task_budget_projects(uuid,uuid,uuid,jsonb,uuid);
