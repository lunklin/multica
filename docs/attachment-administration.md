# Bounded attachment administration (candidate, not deployed)

This change implements the CHEE-199 approved management preview. It does not
authorize deployment, migration execution, replacement of an installed CLI,
or removal of the incident's real attachments. The product ledger is unchanged.

## Source and compatibility

The fixed base is `a2498d7ca815bb1c7aa32ebdb3f41022d0378b25`, the revision
declared by the operator's backend image. Git ancestry verifies it already
contains installed CLI revision `9ff3eb7cf001978838f9f58661601e234158c3aa`.
Image labels are not proof of byte-identical source or a signed registry image.
Existing upload, download, local version, and legacy delete behavior is retained.
The new management route is separately gated and never falls back to legacy DELETE.

## Build and installation reference

The full server and native CLI require the unchanged `server/go.mod` toolchain
(Go 1.26.9) and locked dependencies. From the repository root, on an approved
build host with those prerequisites already available:

```sh
cd server
ADMIN_SOURCE_COMMIT="$(git rev-parse HEAD)"
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go build -ldflags "-X main.version=0.7.2-bounded.attachment-admin-candidate -X main.commit=$ADMIN_SOURCE_COMMIT" -o ./out/multica ./cmd/multica
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go build -ldflags "-X main.version=0.7.2-bounded.attachment-admin-candidate -X main.commit=$ADMIN_SOURCE_COMMIT" -o ./out/server ./cmd/server
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go build -o ./out/multica-attachment-admin ./cmd/multica-attachment-admin
```

Installation would copy the approved binary to an operator-selected directory
(for example `install -m 755 ./out/multica /approved/bin/multica`). This is
documentation, **not an executed installation instruction for this delivery**.
Server rollout and candidate migrations 602/603 need a separate deployment
preview and approval; never run migrations automatically from these commands.
Rollbacks refuse to discard recorded operations. Do not restore removed payloads.

The small `multica-attachment-admin` distribution uses the same `RunCLI` and
HTTP business contract as the native `multica` command bindings, with no daemon
or profile handling. It is useful for bounded isolated verification. It is not
evidence that the full native CLI/server package compiled or integrated.

## Native commands

```sh
multica version --server --output json
multica attachment inspect ATTACHMENT_UUID --output json
multica attachment delete ATTACHMENT_UUID --dry-run --scope-file scope.json --output json
multica attachment delete ATTACHMENT_UUID --confirm-plan PLAN_UUID --request-id REQUEST_UUID --output json
multica attachment operation get REQUEST_UUID --output json
```

Only an authenticated **human workspace Owner** may use the new route. A task
token, cloud machine credential, agent/task context, non-Owner, or another
workspace is denied. Use the normal human login context after an approved
installation. Do not borrow a human profile/token inside an agent run.

The standalone executable takes the same commands and uses environment-injected
`MULTICA_SERVER_URL`, `MULTICA_WORKSPACE_ID`, `MULTICA_TOKEN`. It does not read
profiles. It accepts no credential arguments; do not paste tokens into a URL,
command, report, or test artifact. Help and local version require no network.

`scope.json` is a strict object with `workspace_id`, `issue_id`, and
`attachment_ids` containing exactly 16 different UUIDs. It is an explicit
allowlist, not a glob, filename search or parent-delete operation. The incident's
historical metadata preview is not automatically an executable scope file:
the human must reconcile its exact 16 IDs against inspect metadata. All test
IDs in this change are different synthetic IDs; no real target is hardcoded.

Dry-run persists one metadata-only plan for one target, bound to the full
allowlist, Owner, workspace, issue, target metadata fingerprint, server build
and commit, capability version, and a 10-minute expiry. It does not remove
attachments or objects. Confirmation rechecks membership and target under the
transaction's existing issue -> comment -> attachment lock order. Drift,
expired/version-changed plans and captured-context targets are refused.
Captured-context historical copies remain immutable; no parent-delete workaround.
Private chat/task uploads outside the issue scope are not managed by this route.

## Receipts and ambiguous outcomes

The candidate PostgreSQL adapter commits the request receipt, private cleanup
location and attachment-row deletion **in the same transaction**. Comments,
replies, other attachments and incident history are not deleted or rewritten.
Existing `DeleteAttachment` query increments owner revisions as before.
Workspace serialization plus a globally unique `(kind,id)` index protects
request reentry; the unique index must be present/valid before capability use.

Public receipts never contain the object URL/key, download capability, payload,
credential or credential hash. These cleanup locations remain server-private
in the recovery record. Normal database backup/access controls must include
this new table; no new ACL is introduced by this candidate.
Receipts include server build/commit and `cli_version_reported`; only the
known candidate CLI header value is retained, otherwise UNKNOWN. This client
label is not a cryptographic executable or identity attestation. Empty/unknown
server build or commit disables management operations.

| State | Meaning and next step |
| --- | --- |
| `REJECTED_NO_CHANGE` | This authorized request failed a plan/drift/immutable precondition before deletion. Read its reason; a different plan needs a new request ID. |
| `METADATA_REMOVED_OBJECT_PENDING` | Metadata removal committed; object cleanup or absence verification is incomplete. Read this same request ID, then explicitly resume the same plan/request. |
| `REMOVED_VERIFIED` | Metadata removal committed and the primary object's absence was verified. This does not claim CDN, old URL, object versions or backups erased. |
| Missing/error/transport failure | Outcome UNKNOWN, not proof of removal. Read the original request ID. Do not send a fresh delete ID to resolve uncertainty. |

Object cleanup is idempotent. A crash after metadata commit or after object
deletion leaves a durable recipe; same-ID confirmation resumes it. Already
completed requests return their receipt. Confirmation after timeout first
requires reading the original operation; the CLI does not automatically retry
POST or downgrade to bare DELETE. There is no background sweep/poller.

Local primary verification uses `lstat` for the object, sidecar and staging
file. S3-compatible verification uses authenticated `HeadObject`: only 404
means absent; 200, 403, timeout and other errors do not verify. No object body
is fetched. Unknown storage backends remain pending. This is a primary-key
check, not a claim about historical versions, caches, backups or all copies.

Exit codes align with native CLI tiers: 0 successful read/preview or verified
confirmation; 1 failed/pending/unknown; 2 transport; 3 authentication/permission;
4 missing (not a removal receipt); 5 validation/drift/expired/immutable/version.
A successful `operation get` exits 0 even for a pending receipt: inspect `state`.

## Validation layers and limits

The delivery records the actual commands and streams outside the repository.
The shared dependency-free core and small formal executable can be compiled
with existing Go 1.25.6 using an isolated GOPATH source link and `GO111MODULE=off`.
This does not change `go.mod`, vendor dependencies, or install a CLI.

```sh
GO111MODULE=off go test -race -count=1 -v ./server/internal/attachmentadmin
# Build the standalone command in an isolated GOPATH, then run actual CLI tests:
GO111MODULE=off ADMIN_BINARY=/absolute/path/to/multica-attachment-admin \
  go test -race -count=1 -v ./server/internal/attachmentadmin
GO111MODULE=off go test -race -count=1 -v \
  server/internal/storage/local.go server/internal/storage/util.go \
  server/internal/storage/deletion_verify_test.go
```

The CLI tests execute the delivered executable against an own short-lived
httptest server and synthetic identity resolver. They cover normal and unbound
targets, refusals, immutable targets, storage partial failure, readback/reentry,
and a committed response lost on the actual HTTP connection. The core tests
exercise rollback, ambiguous commit, concurrent same-ID requests and a child
process exiting after a disk-fixture commit, followed by reload/resume.
The disk transaction/identity/object fixtures are **synthetic contracts**,
not PostgreSQL, real authentication, S3 or OS crash/fsync proofs.

The full native Cobra binding, real auth middleware, PostgreSQL transaction/
locking/migrations and S3 HEAD adapter require integration validation. They
were NOT RUN under the available Go 1.25.6/no-download/no-new-environment limits.
An S3 httptest regression is supplied for the approved full-toolchain check.
Real deletion, deployment, installation, CDN/backup erasure and independent
non-author acceptance are not implied by author self-tests.
