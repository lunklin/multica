# Codex safety-review usage

Codex 0.166.0 creates `subagent.other=guardian` sessions for automatic safety
reviews even when ordinary multi-agent features are disabled. These reviews
are not Multica queue admissions. They must remain enabled and their inference
belongs in the parent Run's usage and provider reconciliation.

The adapter keeps the parent JSON-RPC counters authoritative for the parent.
It adds only task-local rollouts whose canonical `session_meta` names both the
exact parent thread and the Guardian source. Per-model usage remains separate;
a missing review model is reported as `unknown`, never guessed from the parent.
This has been checked against the installed parent-only RPC contract and saved
provider records. A future protocol that includes children in parent counters
needs a new reconciliation check before this accounting assumption changes.

Review totals use the execution's start and end times, subtract restored
history, and deduplicate copies by canonical session ID. The explicit task
home is required: this supplementary scan never discovers an ambient user's
history. An additional Run transcript message reports the number of accounted
review sessions and their model/token breakdown. That message does not replace
the agent's functional deliverable.

Rollouts do not provide invoice prices. Adding unpriced review tokens clears a
partial same-model parent price rather than displaying that price as the full
cost. Unknown cost remains unknown; subscription credits are not USD.

The workspace/project allowance counts new Multica queue rows. Safety-review
inference is accounted separately and included in tokens/cost reconciliation;
it does not consume a Multica admission. Neither a six-run allowance nor this
usage correction is a token, model-request, or currency ceiling. The installed
app-server has no verified pre-inference Guardian quota hook. Do not remove
safety review or call a post-start notification a hard pre-admission gate.

Verification uses `TestCodexGuardianUsage*`,
`TestCodexResultUsageIncludesOwnedGuardian`,
`TestCodexExecutePublishesGuardianUsageBreakdown`, the existing usage contract
suite, and the repository real-agent CLI guard. Historical-rollout replay can
verify provider reconciliation without another inference call. It does not
rewrite prior database usage automatically.
