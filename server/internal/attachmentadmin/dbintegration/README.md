# Attachment admin database integration candidate

This directory supplies the missing **test source**, not a deployed capability.
It is a separate Go test package with the `attachmentadminintegration` build tag.
Importing the production `handler` library does not import that package's
`handler_test.go` or execute its TestMain. There is no server listener, migration
runner, database initializer, Docker command, or product permission bypass here.

The source-only snapshots 7ee/98e recorded formatting, compilation and dynamic
checks as NOT RUN. Subsequent execution follows the issue's current bounded
engineering contract; those historical snapshots are not runtime evidence.
The previously accepted e278 formal CLI SYN44 controls are a different layer.
This harness never grants permission to use a real or shared database.

## Entry and output contract

The deliverable is a developer verification binary built by the official Go
test tool. It is not a new attachment business CLI: the existing production
`multica attachment admin` command continues to use the shared service. No
verification flag is added to that command or to the production server.

Examples below are **documentation only**, not installation or execution receipts.
Use the already approved private Go absolute path and locked caches, with
`GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`, `GOVCS=*:off`,
`GOENV=off`, `GOAUTH=off`, `GOFLAGS='-p=1 -mod=readonly'`, `GOMAXPROCS=2`,
`GOMEMLIMIT=1GiB`, and private GOCACHE/GOMODCACHE/GOTMPDIR/GOPATH/out paths.
Do not download or install anything to fill a missing dependency.

From `server/`, after a new compile-only authorization:

```sh
"$PRIVATE_GO" test -c -tags=attachmentadminintegration \
  -o "$PRIVATE_OUT/dbintegration.test" ./internal/attachmentadmin/dbintegration
```

`go test -run '^$' ./internal/handler` is **not** compile-only and is not an
alternative. The existing handler/server TestMains can contact default databases,
modify fixtures and even return Exit0 when no database is available.

The compiled verification binary provides:

```sh
"$PRIVATE_OUT/dbintegration.test" -syn-help
"$PRIVATE_OUT/dbintegration.test" -syn-version
"$PRIVATE_OUT/dbintegration.test" -syn-offline -test.run '^TestTargetContract$'
```

Help/version perform no file, environment, socket or database inspection.
Offline mode only accepts that exact test selection, with no target or child
mode, and exercises missing/unknown/non-SYN/symlink/public receipt rejection.
It also invokes the same compiled entry with skip/list/count=0 for offline, DB
and child modes, checking the specific Exit78 refusal before target access;
these nine owned children cannot reach a DB. A missing SYN executable covers
startup-error receipt fields. It creates only its own temporary SYN input files.
There is no automatic skip from the DB suite to offline mode. In every execution
mode, test.skip and test.list must be empty and test.count must equal1; otherwise
Exit78 precedes any target access or m.Run. Help/version are explicit metadata
commands and never emit a test PASS. Additional SYN controls use fixed /bin/sh
scripts for success, exit23 and a CommandContext timeout; their public sentinel
stdout/stderr include invalid UTF-8 bytes to check lossless receipt encoding.
No shell text comes from a target, credential or user input. These are owned
child-process controls, not PostgreSQL/OS crash tests.

For a separately approved, initialized dedicated PostgreSQL 17 target only:

```sh
env -i PATH=/usr/bin:/bin TMPDIR="$SYN_ROOT/tmp" \
  GOMAXPROCS=2 GOMEMLIMIT=1GiB \
  "$PRIVATE_OUT/dbintegration.test" \
  -syn-target "$SYN_ROOT/INIT-RECEIPT.json" \
  -syn-opt-in CHEE-199-DEDICATED-SYN-ONLY -test.timeout=10m -test.v
```

Exit0 means the selected assertions passed; Exit1 means an assertion failed;
Exit78 means **NOT RUN**, because target/environment validation failed. Exit86
is expected only for the owned post-commit abrupt-exit child; the parent must
observe it and recover the pending operation before passing. A child helper
returning normally is not a separate business control. The parent collects all
children in foreground with a 20-second context per child. Future execution must
also enforce the actually approved overall deadline and exact-owned PID cleanup
rules; it may not extend a prior window.

`-syn-help`, `-syn-version`, target refusals and the final overall result use
JSON on stdout. Per-test Go events can be recorded as JSON using the existing
private Go `tool test2json -t -p attachmentadmin/dbintegration <binary> <flags>`.
Preserve its actual exit, stdout and stderr separately. Do not dump environment,
Authorization headers, tokens, DSNs or raw Auth bodies into any transcript.
Only public SYN signing material and newly generated SYN task tokens are used;
the harness discards production slog output and does not serialize those tokens.

Every continuation/worker path now calls the same recordChild helper **before**
checking exit status or calling Fatal. Its SYN_CHILD_RECEIPT log carries actual
argv/cwd/UTC, available PID, nullable exit code, terminal/run/context error state,
and lossless Base64 stdout/stderr. A failed start has no invented PID/code; a
signal terminal with no numeric exit remains null. A failed parent test exposes
its log even without -test.v; the documented verbose run preserves success logs
too. Capturing buffers alone is not evidence publication. No child is spawned
by merely reading these sources; runtime claims require actual captured results.

## Dedicated target contract

No DATABASE_URL, PG service, TCP, password, localhost or production configuration
fallback exists. Ambient `PG*`, `MULTICA_*`, `AWS_*`, `LOCAL_UPLOAD_*`, `LLM_*`,
DATABASE_URL or JWT_SECRET settings cause Exit78 before connecting. The sole
accepted endpoint is a Unix socket below a private, canonical, non-symlink SYN
root owned by the current UID. The harness creates no database and grants no role.

A **separately authorized initializer**, not an arbitrary existing server,
must supply a 0600 `INIT-RECEIPT.json` under a 0700 root with this format:

```json
{
  "format": 1,
  "purpose": "CHEE-199-DEDICATED-SYN-ONLY",
  "root": "/absolute/private/dedicated-SYN-root",
  "nonce": "64-lowercase-hex-initializer-nonce",
  "database": "chee199_syn_<32 lowercase hex>",
  "role": "chee199_syn_<same 32 lowercase hex>",
  "port": 55439,
  "pid": 12345,
  "system_identifier": "initializer-observed-PG-system-identifier",
  "root_device": 1,
  "root_inode": 2,
  "data_device": 1,
  "data_inode": 3,
  "schema_sha256": "see schema-source.json",
  "expires_at": "explicit-approved-window-end-UTC"
}
```

This is an intentionally non-executable example: placeholders are rejected.
The initializer records actual identities of root/pgdata, the newly started
postmaster, its socket lock, system identifier and explicit database/role. Root,
pgdata and socket directories must be 0700; pgdata/postmaster.pid and the socket
lock must name the same PID and data directory. The receipt expires within two
hours and cannot extend the owner's actual shorter approved window.

The first SQL on every new connection is read-only: current database/user,
`pg_control_system()` system identifier, actual data_directory and major17,
then exactly one matching `chee199_syn_target` marker, the exact migration
ledger, and the candidate's valid unique index. Only then can fixtures write.
The SYN role needs permission for these read-only identity checks and fixture
tables; this is not a reason to grant anything on an existing server. Statement,
lock and idle-transaction timeouts are bounded. No failed check probes a second
target, creates tables, resets data or repairs a schema.

`schema-source.json` pins all 628 official `.up.sql` bytes at e278, including
candidate 602/603. `schema_pin_test.go` embeds their names for exact ledger
comparison. `target-marker.sql` is initializer source only; it is never applied
by the harness. A future initializer must use the repository's migration runner
and its conditional hooks, preserve the separate CONCURRENTLY index boundary,
and record the input digest before filling the marker. A matching marker/ledger
is an initialization-chain check, not proof that every conditional migration or
catalog object was independently verified. The tests still have to exercise the
actual adapter. Local receipts are not cryptographic or OS attestation.

## Requirement to source mapping

| Original B requirement | Candidate entry and observable assertions |
| --- | --- |
| Real shared human/Owner gate | `TestHumanMembershipAndTaskBinding`: real Auth middleware, persisted membership and task token bound to the SYN Owner; member, foreign workspace and task paths rejected with zero plan/business delta. JWT authentication is a SYN signature, not a true human login. |
| Private plan and durable receipt | `TestOwnerAndPrivatePlan`: actual private wrapper namespace in PostgreSQL, no public locator, normal Local deletion, operation get, same-ID reentry, exactly one comment revision increment. |
| Global request identity | `TestRequestIDCannotCrossWorkspaces`: a second workspace cannot overwrite an existing request receipt or delete its own target with that request ID. |
| Captured/expiry/build/drift | `TestCapturedExpiryBuildAndDrift`: original physical state and object retained with precise rejection, unknown build refuses before service use. Mutation of SYN plan time/metadata is explicit fixture injection. |
| Receipt + deletion atomicity | `TestMetadataAndReceiptRollbackTogether`: real callback SQL tentatively contains the operation and deleted metadata; pre-commit fault returns error, production defer rolls back both. Full attachment/comment/issue before image must return. |
| Unknown commit result | `TestLostCommitResponseAndNewProcessResume`: real commit succeeds but caller receives failure; read the original request ID, then a fresh process reads persisted recipe and resumes without a new request. |
| Process boundary | `TestWorkerExitAfterCommit`: owned child exits86 directly after real commit, before storage cleanup; parent observes durable pending, waits child terminal and starts a fresh continuation child. This is client process death, **not** PostgreSQL crash, OS power loss or server restart. |
| Two connections | `TestTwoConnectionsCompete`: independent verified pools and observed backend PIDs; same request converges, two different requests yield one durable deletion, original comments/replies/other attachment survive. |
| Storage partial / namespace | `TestStoragePendingAndOriginalNamespace`: a fault wrapper fails only Delete on a real Local binding; pending receipt persists. Same URL against a replacement directory must not delete it or infer completion; original directory can resume. |
| Missing target | `TestTargetContract`, loadTarget and TestMain: no default or Exit0 skip; explicit offline negative selection; otherwise Exit78 NOT RUN. |

Actual route chain is production Auth → RequireWorkspaceMember →
RequireHumanActor → Handler.AttachmentAdmin → shared HTTP/Service → adminStore /
adminTx → pgx / LocalStorage. The harness reconstructs that small route chain
in memory with httptest recorders. It does **not** run cmd/server's entire router,
listen on HTTP, verify a deployed binary, invoke real user login, exercise S3,
or certify Linux. SYN task/agent records are fixture rows only; no agent runner,
daemon, event subscriber, email, LLM or notification worker is started.

Fixtures use `testutil` builders and `dbee0199-*` IDs, entirely different from
the incident16. They include an original comment/reply and an additional
attachment representing a preserved replacement. Full rejection/rollback images
include original issue/comment revisions; successful removal may bump revisions
once through the existing generated query, while other fields remain identical.
Cleanup removes only own fixture records/files. Standard fixture cleanup ignores
some DB deletion errors; a future isolated target must be disposed only by its
authorized owner, and cleanup/dedicated-instance shutdown require separate
receipts. Never reuse this harness against a shared database.

## Remaining execution dependencies and bounded preview

1. **First independent source review**, then a new bounded formatter/compile and
   offline-negative window using the already private Go/cache, no downloads,
   no DB and no old44 rerun. Proposed maximum: 20 minutes, existing+new private
   files ≤4GiB. Missing locked cache is a stop, not permission to fetch. This
   source delivery has not requested or consumed that new window.
2. Before real integration, a separate concrete environment approval must name
   an available, fixed PostgreSQL17 binary and provenance, private root, runtime
   quota, initialization/migration runner, SYN-only role and data/system identity
   receipts, duration and exact stop/cleanup owner. Those physical inputs are
   **not supplied or authorized** here. No Docker/VM/install/network fallback is
   present. Future initialization must record all conditional schema decisions;
   a fabricated receipt is not an initializer. Cost/time remain UNKNOWN until
   those actual inputs are selected.
3. The dedicated command has no database restart controller. True PostgreSQL
   crash/restart, Linux runtime, S3 trusted binding, deployed router/Auth and
   actual incident16 disposition remain NOT RUN / NOT IMPLEMENTED as applicable.
   Source assertions do not close them. Do not reduce B or product acceptance.

All earlier scopes and evidence remain: original16 approval is not deployment,
S3 refusal is not S3 support, A/B credential remediation is not repeated, and
the pixiu product, original four services, model routing and existing CLI/server
are unchanged. PR/CI/deployment are N/A for this local-only candidate.
