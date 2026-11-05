-- Removing admission while a budget exists could silently restart paid work.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM workspace_run_budget) THEN
        RAISE EXCEPTION 'remove workspace run budgets explicitly before rolling back admission';
    END IF;
END $$;
DROP TRIGGER IF EXISTS zz_admit_workspace_run ON agent_task_queue;
DROP TRIGGER IF EXISTS zz_count_admitted_workspace_run ON agent_task_queue;
DROP FUNCTION IF EXISTS admit_workspace_run();
DROP FUNCTION IF EXISTS count_admitted_workspace_run();
