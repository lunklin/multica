# Project run budgets

A human workspace owner/admin can arm a separate allowance for a project.
This is useful for a development-entry project in a shared workspace: an
exhausted allowance must not stop unrelated project work. Unconfigured
projects keep their existing behavior. Workspace and project allowances may
coexist; an admission must satisfy every applicable allowance atomically.

```sh
multica project run-budget get PROJECT
multica project run-budget set PROJECT --max-runs 6 \
  --deadline 2026-11-05T18:00:00+08:00 --expected-revision 0
```

GET/PUT `/api/projects/{id}/run-budget` use native project resolution and
workspace membership. Agents may read, but only human owners/admins may arm,
pause or extend an allowance. Revisions provide compare-and-swap; updates never
reset admitted runs and cannot lower the total below admitted usage. Initial
arming takes workspace then project locks and refuses unfinished scoped work.
Other projects' unfinished work does not prevent this project from arming.

## Admission ownership

Every new queue row stores immutable `budget_project_ids`. The database derives
these from the issue, project chat, quick-create or autopilot, plus all recorded
parent/retry/rerun/delegation/recovery/wakeup sources. Crossing into another
project retains the source and destination scopes; every configured budget
is charged. A request cannot replace the snapshot with its own JSON or clear
it after insertion. Cancellation, failure, deletion and re-claim never refund
an admission. Conflicts and transaction rollbacks never consume one.

Task authentication supplies a private context value containing the verified
caller task ID. Native enqueue services persist that source even when an agent
changes an existing issue or sends an unscoped chat. Client-supplied headers
cannot create this context. Human callers retain their existing attribution.
Safety reviews are separately accounted, included in token/cost reconciliation,
and do not consume a Multica dispatch allowance.

Legacy queue rows retain NULL snapshots and resolve their directly recorded
project for the initial unfinished-work check. A new task with no project gets
an empty array, which is distinct from legacy data. Moving an issue or changing
a chat's project does not relabel an existing admission. Human work started
without a project or a scoped ancestor remains unscoped; start the controlled
flow from its project issue/chat rather than a global unscoped conversation.

A project-budgeted agent cannot create or change persistent Autopilot rules,
which could outlive the recorded source admission. Human rule management and
unbudgeted callers keep their current behavior; immediate dispatches still
inherit and consume applicable scopes. Agents cannot delete a budgeted project;
the check runs under the same project lock as human initial arming. Human
project deletion cleans up its budget in the existing transaction.

## Stop, recovery and deployment

The limit counts new Multica queue rows, including queued, deferred, failed and
retry attempts. It does not limit inference calls, tokens or currency, cancel
already admitted work, or automatically change Issue business status. Inspect
`stop_reason`, preserve receipts and use the existing blocked/human-check
workflow states. A human may extend the total or move the deadline without
rewriting history. See `codex-guardian-usage.md` for native safety-review usage.

Migrations 598–600 add the table and nullable admission snapshot, create the
project unique index concurrently, then install the shared queue gate. Apply
them before updating the application, outside the deployment queue SHARE fence.
Application rollback keeps schema and configured protections. The down migration
refuses to remove admission while project budgets exist. No foreign keys or
cascading deletes are introduced.

Verification covers native queue paths, authenticated caller propagation,
concurrent admission, cross-project inheritance, two-policy atomicity, batch and
transaction rollback, immutable snapshots, owner/agent authority, project-delete
versus arming races, normal legacy upgrade and native six-handoff HTTP replay.
These offline checks do not authorize another paid acceptance run.
