# Remaining attachment administration verification

This is a bounded verification plan, not deployment authorization. The production
implementation at `f9ba31aeb7607b7f5b814645de1270f71ec63a55` has two narrowly
closed findings: ATT-REV-01 for local/SYN recovery and ATT-REV-02 for the shared
CLI contract. The whole feature, native integration and real disposition remain
unaccepted. Later test preparation does not extend that verdict to a new head.

## Prepared CLI harness

The same behavioral tests accept one explicit candidate path:

- `ADMIN_BINARY`: the dependency-free standalone CLI distribution.
- `ADMIN_NATIVE_BINARY`: a candidate built from `server/cmd/multica`, not an
  installed CLI discovered from PATH.

The native mode adds JSON output to the local version call. Each CLI child runs
in its own test directory with an explicit environment containing only the
synthetic loopback URL, workspace and token; it does not inherit HOME, profile,
daemon/task variables or real credentials. The HTTP server still uses a clearly
synthetic identity resolver and transaction fixture. This is native command
wiring coverage, not real JWT, workspace membership or PostgreSQL evidence.

Standalone mode has been rerun with the existing Go 1.25.6. Native mode remains
NOT RUN until the full candidate can be built with a compatible toolchain.

## Proposed first stage (requires separate environment approval)

Use a private task-owned toolchain/module/build cache and output directory.
The minimum source requirement is Go 1.26.9. A specific supported 1.26-series
archive must be pinned from official release metadata, SHA/size verified before
extraction, without replacing system Go, installed Multica or any service.
Keep `go.mod` and `go.sum` unchanged. Fetch only locked public modules through
the official Go module proxy/checksum service, without direct/private-repo
fallback, credentials, `go get`, `tidy`, dependency upgrades or global settings.

After explicit approval and prerequisites, the checks are:

1. Build candidate `./cmd/multica` and `./cmd/server` to the private output
   directory, with fixed version/commit metadata. Do not run the server.
2. Compile the handler/storage/server test binaries using `go test -c` and keep
   their paths. Do not execute DB-backed package test binaries in this stage.
3. Run the shared-core tests and the prepared CLI harness against the newly
   built native candidate with `ADMIN_NATIVE_BINARY`. Only the owned httptest
   server is permitted; record individual CLI output and exit codes.
4. Check the narrow synthetic Auth/task-source middleware tests only after
   reviewing package initialization and TestMain behavior; use no real key or
   service. Record the exact layer tested, not a full Auth acceptance.
5. Read back source/dependency/binary fingerprints and return findings to the
   original Writer. No push, CI, install, daemon update, migration or deployment.

## Why compile-only is necessary before a dedicated test database exists

`server/internal/handler/handler_test.go` has a TestMain that defaults to
`localhost:5432/multica` when DATABASE_URL is empty. It can create and clean up
fixtures before selected tests run; a failed DB connection exits zero. Therefore
`go test -run '^$'` is not a safe compile-only substitute, and exit zero alone
is not proof those tests executed. This plan uses `go test -c` without running
TestMain. Do not point the suite at the existing Multica/Pixiu database, and do
not use a default/assumed local database.

## Subsequent dedicated PostgreSQL stage (not authorized or implemented here)

Use an explicitly selected disposable PostgreSQL 17 test environment, owned
synthetic subjects/workspaces, fresh schema fixtures and a fail-closed target
check. It must have no connection to the existing platform/Pixiu data, volumes,
credentials or services. The test runner must not fall back to DATABASE_URL or
localhost defaults. It needs an explicit opt-in and must fail, not silently
skip as PASS, when prerequisites are missing. Bootstrap/DDL, processes, resource
caps and teardown require their own concrete approved preview; this document
does not create a runnable database setup task.

The missing DB/handler integration harness should assert externally observable
outcomes for these exact cases:

| Case | Required observations |
| --- | --- |
| Human Owner preview/confirm | Real persisted private plan wrapper roundtrip, metadata-only preview, exact target removal and retrievable operation; comments/replies/other attachments unchanged. |
| Agent/task, non-Owner, cross-workspace | Real auth/membership path denies; no plan/deletion/object change, without borrowing an Owner profile. |
| Scope/expiry/build/metadata drift and captured context | Rejection before deletion; exact ID and reason, no parent deletion. |
| Failure between receipt insertion and row deletion | PostgreSQL rolls both back; no dangling accepted receipt or storage deletion. |
| Commit response lost, reopen and same-ID resume | Query original request ID, one metadata delta, durable namespace/recipe; no new blind delete. |
| Concurrent same-ID/different-ID confirmation | Unique-key and owner locks produce the documented single-target outcome; no lost receipt or owner-lock inversion. |
| Storage failure/namespace drift | Durable pending retains original location proof; another empty root never verifies completion. |
| Real process boundary | New connection/process reads committed recipe, closes its owned resources and completes only the original namespace. Distinguish this from power-loss durability. |

## Provider scope

The existing local path implementation is the smallest accepted source branch.
The server can choose S3 before falling back to LocalStorage, so provider support
must be checked rather than assumed. Relative `/uploads/` metadata is consistent
with LocalStorage in the fixed source, but does not prove current environment
configuration or byte-identical deployment. Captured-context eligibility and
the original 16 targets still need native readback after a separately approved
rollout.

S3 has no trusted namespace binder in this candidate; preview rejects it before
metadata changes. S3 HEAD or matching bucket/endpoint text is not a namespace
proof. Implementing S3 requires a specific trusted provider identity contract
and controlled conformance tests. Do not guess one, create a sentinel/IAM change,
or automatically expand the original 16-target task into all storage providers.
This remains an explicit engineering gap, not an encryption/Owner-approval excuse.
