# Delegated failures and workflow ownership

A failed delegated task keeps its original system recovery signal and failed-task
evidence. On a project with a named workflow, or a Default workflow with handoffs,
automatic coordination resumes only when the original delegator still runs for
the issue's effective assignee. Explicit agent assignments override a column's
default handler. A squad recovery must still belong to that squad and its current
leader. This matches the claim-time assignee/observer distinction.
Existing runnable coverage and delivered recovery receipts take precedence:
later ownership changes do not produce a second human escalation for an
already handled failure.

Final lifecycle, coverage and effective-owner checks share the same issue-row
lock transaction as merging recovery inputs or inserting/registering a task.
An ownership update either commits before that locked check or waits until the
admission commits. Runtime MCP preparation happens outside the transaction;
the source squad row is locked before the agent and issue so leader rotation
cannot invalidate squad ownership during admission. The settled signal is read
again under the issue lock, including when a callback holds an older snapshot;
notifications and queue events publish after commit. This path fails closed
without a transaction starter instead of relying on the general service helper's
autocommit fallback.

An observer recovery stops before merging inputs or admitting another task. In
one transaction, the service marks the existing signal `recovery_settled_at`,
writes a system explanation and creates an eligible member's existing
`task_failed` / `action_required` inbox item with `outcome: needs_human`.
The signal content, failed task, issue status, assignee and already queued work
are retained. The settlement is the idempotency boundary: repeated callbacks or
sweeps do not create another run or notification. Changing ownership later does
not reopen that stopped recovery; a human decides whether to resume work.

This does not move the issue into a blocked column or transfer writer authority
to a reviewer. Unscoped issues, Default projects without handoffs and valid
coordinator continuations keep their existing behavior. There is no schema,
daemon or protocol change, and no model invocation in this decision.

## Diagnostic files and delivery receipts

Diagnostic redirections and scratch files are writes and must respect the task's
write scope. Do not write CLI response JSON, stderr logs or temporary replies at
the product repository root. Prefer stdout/in-memory parsing; where durable
evidence is required, use the task's explicitly permitted evidence directory
and record its task ID and receipt. An external durable archive is valid only
when separately allowed; do not invent one outside the task's write scope.
A task's `TMPDIR` is removed on exit and cannot serve as a durable failure archive.

These instructions are a role policy, not a filesystem hard gate. The daemon's
existing worktree finalizer still commits all leftover changes to preserve work,
including on failed runs. Distinguish an author's intentional commit from an
automatic preservation commit in review receipts; a preservation commit is not
evidence of review approval or authorization to include its files in delivery.
Do not delete or rewrite an existing failed branch to hide those files.

`LocalWorktree.AbortWithReason` can preserve an uncommitted worktree internally,
but it is not a public preserve/restore API. Extending it to ordinary failures
also requires GC retention and primary-failure reporting changes. Merely skipping
auto-commit does not create a durable, supported recovery path.
