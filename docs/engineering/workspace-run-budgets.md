# Workspace run budgets

A human administrator can arm a durable allowance for **new Multica queue
rows** in one workspace. Use a dedicated acceptance workspace when the limit
belongs to a trial: every issue, chat, quick-create, workflow, wakeup,
autopilot and retry shares the same allowance. Workspaces without a budget
retain their existing behavior.

The admission counter is in the existing Multica database. There is no second
task store. A transaction locks the workspace's budget before inserting a task,
and charges it only after an actual insertion. Concurrent admissions cannot
exceed the allowance. `ON CONFLICT DO NOTHING` and rolled-back transactions do
not spend slots; failure, cancellation, deletion and re-claim do not refund
slots. A new retry or manual rerun does spend a slot.

The BEFORE trigger refuses excess single-row inserts using the existing no-row
contract. In particular, refusing an automatic retry must not roll back its
parent's failed status. An oversized multirow statement is rejected atomically.

## Configure and inspect

```sh
multica workspace run-budget get WORKSPACE
multica workspace run-budget set WORKSPACE --max-runs 6 \
  --deadline 2026-11-04T18:00:00+08:00 --expected-revision 0
```

`GET /api/workspaces/{id}/run-budget` is member-readable, including task-scoped
agents. An absent budget returns `configured=false` and null limit/remaining
fields, not a zero allowance. Configured budgets report admitted/remaining runs,
revision, deadline, pause state and a computed `stop_reason` (`run_limit`,
`deadline` or `paused`), even if a failed transaction could not persist its last
refusal note.

`PUT` requires a **human** owner/admin. Its body contains `max_runs`, `deadline`,
`paused` and `expected_revision`. Revision 0 creates a budget only when no
unfinished runs exist, under the same workspace row fence used by queue
writers. Existing terminal history is not part of this new allowance. Updates
require the current revision, preserve `admitted_runs` and cannot set a limit
below usage. There is no usage-reset or budget-delete API. A human may enlarge
the total allowance or move its deadline to resume work; agents cannot do so.

## Boundaries

This is a **new-task admission** cap. Pausing or expiring it does not cancel
already admitted work, and a queue refusal does not itself change an issue's
business status. Inspect `stop_reason` and hand blocked work to a human; do not
report a refused handoff as completed delivery. Completed receipts, commits and
native task history remain available for recovery.

One Multica run can contain many model requests. This cap is not a dollar,
token or model-turn limit. A trial claiming at most six total Agent executions
must also disable native CLI subagents, verify that control in the actual
runtime/workdir, and use a per-run timeout. Native safety reviewers need separate
accounting: Codex can create internal `subagent.other=guardian` sessions even
when ordinary `multi_agent` features are disabled. These sessions are not
Multica queue admissions and their usage may be absent from the parent Run's
reported totals. Check native session history against the provider ledger
before claiming an all-native-Agent ceiling or complete cost coverage. Keep
security approval controls intact; this admission budget does not authorize
removing them to reduce usage. The minimal six-run review/repair
trial uses one issue and only native status handoffs:

```text
Mika -> engineer -> reviewer RETURN -> engineer -> reviewer PASS -> Mika
                                                              -> human check
```

Do not add equivalent mentions, leader runs, child-completion subscriptions or
external orchestration between these handoffs. Any failed/retried/no-op run
uses one of the same six slots; exhaustion leaves the trial incomplete.

## Upgrade and rollback

Migrations 595-597 are additive. 596 creates its unique index concurrently in
a separate file. Apply migrations before updating the API/CLI. Installing the
queue triggers briefly requires a queue-table DDL lock: do not hold the
separate deployment SHARE fence while applying migration 597. Once migrations
finish, the ordinary no-active-runs deployment fence can protect API restart.

An application rollback keeps these migrations and any budget rows: old enqueue
paths are still guarded. The 597 down migration refuses to remove admission
while budgets exist. Explicitly drain and remove scoped budgets only as a
separately reviewed schema rollback; never silently remove a paid-work cap.
Workspace deletion removes its budget in the existing application cleanup plan.

Verification uses the repository's real-agent CLI guard, an explicit disposable
`DATABASE_URL`, `TestWorkspaceRunBudget*`,
`TestBoundedDeliveryHandoffUsesSixAdmissions`, and the full backend race suite.
Synthetic state-machine tests do not establish real model delivery quality.
