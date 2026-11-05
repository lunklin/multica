-- name: LockQoderTaskIssueForLifecycle :one
-- Serialize Qoder lifecycle before its budget/task locks. Do not condition this
-- lock on current ownership: syncQoderIssue rechecks that after the Task CAS.
WITH workspace_lock AS MATERIALIZED (
    SELECT w.id FROM workspace w
    JOIN agent a ON a.workspace_id=w.id
    JOIN agent_task_queue t ON t.agent_id=a.id
    JOIN issue i ON i.id=t.issue_id AND i.workspace_id=w.id
    JOIN agent_runtime r ON r.id=t.runtime_id AND r.workspace_id=w.id
    WHERE t.id = @task_id AND r.provider='qoder_cloud'
    FOR KEY SHARE OF w
), chat_lock AS MATERIALIZED (
    SELECT cs.id FROM agent_task_queue t
    JOIN agent a ON a.id=t.agent_id
    JOIN issue i ON i.id=t.issue_id AND i.workspace_id=a.workspace_id
    JOIN agent_runtime r ON r.id=t.runtime_id AND r.workspace_id=i.workspace_id
    JOIN chat_session cs ON cs.id=t.chat_session_id AND cs.workspace_id=i.workspace_id
    WHERE t.id = @task_id AND r.provider='qoder_cloud'
      AND (SELECT count(*) FROM workspace_lock)>=0
    FOR UPDATE OF cs
)
SELECT i.id FROM agent_task_queue t
JOIN agent a ON a.id=t.agent_id
JOIN issue i ON i.id=t.issue_id AND i.workspace_id=a.workspace_id
JOIN agent_runtime r ON r.id=t.runtime_id AND r.workspace_id=i.workspace_id
WHERE t.id = @task_id AND r.provider='qoder_cloud'
  AND (SELECT count(*) FROM workspace_lock)>=0
  AND (SELECT count(*) FROM chat_lock)>=0
FOR UPDATE OF i;

-- name: LockQoderTaskIssue :one
-- Lifecycle has already serialized the issue before taking its Task lock.
SELECT i.* FROM issue i
JOIN agent_task_queue t ON t.issue_id = i.id
JOIN agent_runtime r ON r.id = t.runtime_id AND r.workspace_id = i.workspace_id
WHERE t.id = sqlc.arg('task_id') AND r.provider = 'qoder_cloud'
  AND i.assignee_type = 'agent' AND i.assignee_id = t.agent_id
  AND NOT EXISTS (
    SELECT 1 FROM agent_task_queue newer
    WHERE newer.issue_id = i.id AND (newer.created_at, newer.id) > (t.created_at, t.id)
  )
FOR UPDATE OF i;

-- name: SetQoderIssueRevision :exec
UPDATE agent_task_queue SET context = jsonb_set(
    COALESCE(context, '{}'::jsonb), '{qoder_issue_revision}', to_jsonb(sqlc.arg('revision')::bigint)
) WHERE id = sqlc.arg('task_id');
