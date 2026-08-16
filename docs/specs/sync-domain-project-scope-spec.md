# Spec: sync domains, project scope, and manual project identities

| | |
|---|---|
| Status | Proposed; the current pre-alpha behavior is documented, but domain fingerprinting, enrollment, and manual-identity consumption remain unfinished |
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

### 2.2 Planned domain fingerprint

Before a device joins an existing domain, the CLI should display a short,
non-secret domain fingerprint derived from the normalized Remote namespace and
the keyfile public identity. The fingerprint must not contain the passphrase,
Recovery Key, storage credentials, local paths, or session contents.

The intended user flow is:

- first init creates and displays the domain fingerprint;
- later init reports that it opened an existing domain and displays the same
  fingerprint;
- a status/doctor command can display the redacted fingerprint;
- an explicit confirmation or expected-fingerprint option prevents accidental
  attachment to the wrong Remote namespace.

A fingerprint confirms which domain a device is opening; it is not, by itself, an
access-control mechanism.

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

### 4.2 Current implementation gap

The project package can identify a manual name and the project command can persist
a binding, but the current push/watch/list/pull/resume paths still call the
automatic Git-based identifier resolver directly. As a result, a no-Git
directory is reported as unstable and a binding alone does not yet make the
normal sync path work.

This is an implementation gap, not an intentional security boundary. The fix
must introduce one shared current-project resolver:

1. use a stable Git-remote identity when one exists;
2. otherwise look up an explicit binding for the current canonical root;
3. fail closed when neither identity exists;
4. return the same identity to push, watch, list, pull, resume, history, status,
   and project-policy checks.

The resolver must never fall back to an absolute path or silently invent a
machine-specific identity.

## 5. Acceptance matrix

| Scenario | Expected result |
|---|---|
| Same Remote namespace/keyfile, two installations | Same domain, different device branches |
| Same bucket, different prefix | Separate domains |
| Existing keyfile, wrong passphrase | Join fails before data access |
| Existing keyfile, unexpected public identity | Configuration/keyfile mismatch fails closed |
| Two Git checkouts of one project | Same project ID when their canonical Git remote matches |
| Two no-Git roots with the same manual identity | Same project namespace after the shared resolver is implemented |
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
