# Spec: sync domains, project scope, and manual project identities

| | |
|---|---|
| Status | Proposed; domain fingerprinting, persisted namespace binding, keyfile validation and manual-identity consumption are implemented, while capability-based enrollment and revocation remain unfinished |
| Date | 2026-08-16 |
| Depends on | cli-init-spec.md, cli-project-spec.md, cli-push-spec.md, cli-watch-spec.md, device-mode-spec.md, crypto-spec.md |

## 1. Purpose

This specification separates three questions that must not be answered by one
identifier:

1. Which devices belong to the same encrypted synchronization domain?
2. Which projects are stored inside that domain?
3. How is a project identified when it has no usable Git remote?

A device ID answers only the second half of the first question: it identifies a
writer branch inside one domain. It is not a membership credential and it is
not a project identity.

## 2. Sync domain

### 2.1 Current pre-alpha boundary

In the current implementation, a sync domain is implicit. It is the combination
of the configured Remote namespace and the Remote keyfile/data-key identity:

- the first init creates the Remote keyfile;
- a later init that finds the keyfile unlocks the existing envelope instead of
  replacing it;
- devices that unlock the same keyfile derive the same identifier key and
  therefore the same project and session namespace;
- every installation still receives its own random device identity and writes
  its own Remote branch.

The same storage bucket with different prefixes is therefore a different
namespace. Two devices using the same namespace, keyfile, and passphrase are
currently treated as members of the same domain.

The keyfile public-identity pin prevents a local configuration from silently
accepting a different encryption identity. It does not implement an invitation
flow or per-device authorization. A person who obtains the Remote credentials
and the passphrase or Recovery Key can join the domain. Device removal currently
deletes that device's Remote objects but does not revoke credentials already held
by the device.

### 2.2 Implemented domain fingerprint

The CLI now displays a short, non-secret domain fingerprint derived from the
normalized Remote namespace and the keyfile public identity. The fingerprint
text does not contain the passphrase, Recovery Key, storage credentials, local
paths, or session contents.

The implemented flow is:

- first init creates the keyfile and displays the domain fingerprint;
- later init opens the existing keyfile and displays the same fingerprint when
  the normalized namespace and public identity match;
- `status` and `doctor` display the redacted fingerprint without contacting the
  backend in their normal local-only mode;
- `init --expect-domain-fingerprint VALUE` rejects a mismatch before a new
  keyfile is published or local configuration is saved;
- new configurations persist the accepted fingerprint in `config.json`;
- core Remote commands compare the current namespace with that binding, and
  push reads only the keyfile object before writing session data;
- `status` and `doctor` report a namespace binding mismatch without treating a
  reachable but different Remote as healthy.

A fingerprint confirms which configured namespace a device is opening and
prevents accidental namespace drift; it is not, by itself, an access-control
mechanism. The expected-fingerprint init flag is the current explicit join
confirmation; it does not grant a one-time capability.

### 2.3 Enrollment and revocation

A production-grade domain needs an explicit invite/join flow. A future invite
should carry, at minimum:

- the Remote namespace;
- the expected domain fingerprint;
- a one-time or expiring enrollment capability;
- the joining device's public identity or a way to register it.

The design must also define what happens after device removal. Deleting a branch
is not enough when the removed device retains material that can read future
objects. Per-device key wrapping, a domain-key rotation, or an equivalent
revocation mechanism is required before claiming strong membership control.

## 3. Multiple projects in one domain

One sync domain may contain many projects. The project identity is derived
separately from the domain key:

- each stable project identity gets a distinct project ID;
- session IDs are scoped under that project ID;
- project A and project B do not share session history even when they use the
  same Remote and device.

The current command scope is deliberately narrow:

- push scans only the current project;
- watch monitors only the current project;
- a Claude Code hook pushes the project in whose working directory the session
  ended;
- list, pull, resume, and history operate on the current project unless their
  command-specific selector says otherwise;
- there is no implicit scan of every project on the machine and no default
  global all-projects watcher.

Project policy is local and independent of domain membership:

- normal permits local push and explicit remote inspection/restoration;
- push-only permits upload but blocks remote session reads;
- excluded skips synchronization for that project.

Binding a project records a local root-to-identity relationship. It does not
contact the Remote, upload sessions, or automatically enable a global watcher.

## 4. Projects without Git

### 4.1 Stable identity rule

A local absolute path, hostname, or username is not a cross-device project
identity. A project without a usable Git remote therefore needs an explicit
manual identity that is repeated on every device.

The intended form is:

    agentsync project bind --name client-project --path /path/to/project

This produces the stable identity manual:client-project. The same logical
project on another device must use the same manual identity, while different
projects in the same domain must use different names.

Manual identities are user-declared capabilities, not proof that two directories
contain the same files. Workspace fingerprints and restore safety checks remain
responsible for detecting divergent content.

### 4.2 Shared current-project resolver

The shared CLI resolver now applies one rule to the current-project consumers:

1. use a stable Git-remote identity when one exists;
2. otherwise look up an explicit binding for the current canonical root or its
   containing bound root;
3. fail closed when neither identity exists;
4. return the same identity to push, watch, list, pull, resume, history, status,
   remote lifecycle, and project-policy checks.

It never falls back to an absolute path or silently invents a
machine-specific identity. A no-Git directory with no binding remains unstable
and produces an actionable error. A binding with conflicting identities for the
same root is rejected rather than guessed.

## 5. Acceptance matrix

| Scenario | Expected result |
|---|---|
| Same Remote namespace/keyfile, two installations | Same domain, different device branches |
| Same bucket, different prefix | Separate domains |
| Existing keyfile, wrong passphrase | Join fails before data access |
| Existing keyfile, unexpected public identity | Configuration/keyfile mismatch fails closed |
| Two Git checkouts of one project | Same project ID when their canonical Git remote matches |
| Two no-Git roots with the same manual identity | Same project namespace through the shared resolver |
| Two no-Git roots with different manual identities | Separate project namespaces |
| No-Git root without a binding | No push, no remote read, and an actionable error |
| Multiple configured projects | Only the current project is processed by push/watch |
| Excluded project | No upload or remote inspection for that project |
| Push-only project | Upload allowed; remote session reads blocked |

## 6. Non-goals

This specification does not add an AgentSync account, hosted service, telemetry,
or implicit background scanning of user directories. Any all-project operation
must be explicit, policy-filtered, and separately reviewed for privacy and
resource usage.
