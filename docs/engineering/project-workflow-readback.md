# Project workflow readback

Before creating an issue in a project's workflow, a coordinator can read the
actual step keys, handlers, instructions and next/back routes:

```sh
multica project workflow get PROJECT --output json
```

JSON is the default and only output format. `PROJECT` uses the existing project
ID/prefix resolver. The response contains:

- `project_id`: the resolved project.
- `resolved_source`: `project` for an explicit binding, otherwise `default`.
- `workflow`: the complete native workflow object, without removing unknown
  fields or rewriting steps.
- `status_names`: exact step keys mapped to their catalog display names.

Use exact keys for status changes; display names alone are not status identifiers.
An explicitly bound workflow that is unavailable does not fall back to Default.
Unbound projects require exactly one Default. Missing or malformed workflow,
handler or status-name data returns an error without partial JSON.

The command sends only existing GET requests and preserves the CLI's workspace
and task credential headers. It does not configure workflows, create issues or
dispatch agents. The existing workflow-list endpoint can initialize Default in
a workspace that has never had one; that server behavior is unchanged.

This is a CLI-only addition: no endpoint, migration or daemon protocol changes
are required. A successful readback proves API compatibility, not a completed
agent handoff or an independent quality review.
