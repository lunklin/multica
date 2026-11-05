# Project stop on execution failure

`project_run_budget.stop_on_failure` is an optional execution policy, defaulting
to false. An opted-in project's first actual Task transition to `failed` sets
`paused=true`, records the read-only `failure_task_id`, and increments its budget
revision in the same transaction as the failure. Failure reasons and work receipts
remain on the original Task. The latch uses the Task's immutable project scopes:
a cross-project descendant stops every opted-in scope it was admitted against.
Unrelated projects and tenants retain their own budgets.

This is a runtime failure policy. A successful reviewer RETURN or an expected red
unit test reported inside a completed Task does not set the latch. Successful
stage handoffs keep their existing admission behavior. The admission counter is
never refunded, including for failed, held, cancelled or deleted Task rows.

## Atomic boundaries

Every native Task failure SQL freezes its original candidate IDs, locks their
configured project budgets in UUID order, rechecks the original Task predicates,
then latches only rows actually returned by the failed-state CAS. Bounded sweepers
retain `SKIP LOCKED` after the budget fence. Budget locks precede Task row locks;
chat failure keeps the existing chat-session-before-budget order. Qoder start
and completion pre-lock their workspace, optional chat session and issue before
budget/Task locks. This preserves issue-before-budget coordination with recovery
admission while retaining the original assignee, newest-run and revision checks.
Other providers keep their lifecycle behavior. There is no
row-update trigger that acquires a budget after taking the Task lock.

The existing admission gate refuses retries and other new descendants once the
latch commits. Refusal returns no row, preserving the original failure and latch
rather than rolling them back. Failure callbacks also skip retry creation and
recovery merging when their source scopes are latched. Recovery checks the
failed Task's immutable scopes again under its issue/budget transaction fence
before merging or registering inputs, including when an existing pending Task
has a different admission snapshot. The durable recovery signal is retained for
later human decisions.

Claim and reclaim omit failure-latched Tasks. Both native start mutations and
claim ownership locking acquire the same budget fence before taking the Task
lock, then check a fresh latch snapshot. Start-first commits establish an in-flight
Run; a subsequent failure does not kill that Run or reject its running ACK replay.
Failure-first prevents queued, dispatched and local-directory-waiting Tasks from
starting. These Tasks remain held with their admission already charged. A start
check deliberately ignores the normal admission count and deadline: an already
admitted final stage must still be able to start when remaining admissions are zero.

## Human resumption and deployment

An owner/admin must explicitly send `paused:false` through the existing budget
PUT to clear the latch. Omitting `paused`, raising `max_runs`, extending a deadline,
or changing `stop_on_failure` alone does not clear it. Task tokens cannot change
budgets. Clearing the latch retains `admitted_runs`; subsequent failures can set a
new latch, while previous failures remain in Task history and notifications.

Migration 601 adds a boolean and a nullable UUID without a Task foreign key or
new index. The receipt therefore survives Task retention/deletion. Upgrade and
rollback require the matching Backend SQL version. An older Backend does not
implement the failure/start fences; keeping the new columns while rolling back
the executable does not preserve this feature's guarantee. This candidate does
not change or restart production, extend a deadline, reset usage, or authorize
additional model executions.
