# Spec: workspace difference context injection

| | |
|---|---|
| Status | Implemented locally |
| Depends on | `syncflow-restore-apply-spec.md`, `project` workspace reports, adapter JSONL writes |

## 1. Purpose

A restored session can contain an old view of files that are different in the
target workspace. The restore path may therefore append a bounded explanation
so the Agent is prompted to re-read those files before relying on the old
context.

The feature is additive: it never rewrites the canonical remote stream and it
does not change the original restored records.

## 2. Enablement and safety

`syncflow.ApplyRestore` keeps `InjectWorkspaceContext` disabled by default so
library callers opt in explicitly. The `resume` command enables it by default
and exposes `--no-workspace-context` for users who do not want the extra local
record.

The marker is considered only after the normal workspace decision. A divergent
workspace still requires `--allow-divergent`; disabling context injection does
not weaken that safety gate. Consistent workspaces receive no marker.

## 3. Local-only record

The injected JSONL record has this shape:

```json
{"type":"user","isMeta":true,"message":{"content":"..."},"agentsync":{"kind":"workspace-difference","version":1}}
```

The content contains the verdict explanation and sorted path/note pairs from
the structured `project.Report`. Control characters and excessive whitespace
are removed, each field is bounded, at most 64 files are listed, and the whole
record is bounded to 32 KiB.

The record is appended only to the localised stream passed to the adapter's
atomic writer. `CanonicalizeSession` recognises the exact marker kind/version
and filters it before a future push, so the explanation remains device-local
and cannot become remote session history.

## 4. Result and failure behavior

`RestoreApplyResult.ContextInjected` and the `resume` text/JSON reports expose
whether the marker was written. If marker construction fails, no adapter write
is attempted and the error retains `ErrWorkspaceContextInjection`.

The normal atomic create/replace semantics remain unchanged. A failed write
does not create a partial session.

## 5. Test plan

Tests cover marker construction and bounds, filtering during canonicalisation,
explicit divergent restore consent, and the unchanged consistent-workspace
path.