# Spec: `agentsync device` remote management

| | |
|---|---|
| Status | Implemented locally; external backend failure and multi-OS acceptance remain pending |
| Date | 2026-08-16 |
| Depends on | `device-mode-spec.md`, `syncer-pull-state-spec.md`, `cli-pull-spec.md` |

## 1. Invocation

The existing local commands remain available:

```text
agentsync device status [--json]
agentsync device mode normal|push-only|disabled
```

Remote device management adds:

```text
agentsync device list [--json]
agentsync device rename NAME
agentsync device remove DEVICE_ID [--yes]
agentsync device rotate-key
agentsync device invite [--output PATH]
```

`device list` first uses the local device grant in a managed domain and falls
back to an explicitly entered storage passphrase only for bootstrap or recovery.
`device rename`, `device rotate-key` and `device remove` require an active local
device grant; rotate/remove also prompt for the current and new passphrases.
Removing a device always requires an interactive confirmation unless `--yes` is
supplied. The passphrase and Recovery Key are never accepted as command-line
arguments.

`device invite` reads the local configuration and identifier key without
contacting the Remote. It writes a portable JSON package atomically when
`--output PATH` is supplied, or emits the package as JSON on stdout. The package
contains the Remote settings, non-secret domain fingerprint, issuer device
identity, nonce, and proof; it contains no credentials, passphrase, session
content, or private key material.

## 2. Remote device records

Each successful push publishes one encrypted self-description at:

```text
v1/devices/<device-id>
```

The object key is the device's opaque ID; the encrypted plaintext contains only
the record version, display name, operating-system label, and UTC
`lastActiveAt` timestamp. The record is authenticated to its exact key path,
bounded before decryption. Normal CLI paths publish and update only their
local ID; the dumb backend does not provide an ownership ACL.

Push publishes the record after at least one session branch has been written.
A successful `device rename` updates the local configuration and then
publishes the new record. If publication fails after the local atomic save, the
command reports the remote failure; a later push or rename repairs the remote
record.

Older stores may have session branches but no device record. Listing discovers
device IDs from valid session branch object keys and presents those entries with
unknown name/system and an advisory backend activity time. This fallback never
reads or decrypts session bodies.

## 3. Boundaries

The device ID is an opaque namespace identifier. It is shown in list output so
it can be supplied to `device remove`, but it is never treated as a host name,
path, or human identity.

`device list` reads only the encrypted device records and remote object keys
needed to discover legacy branches. It does not read session metadata, shard
bodies, local Agent files, pull cursors, observed tips, or configuration state
beyond the backend setup and local device identity.

`device remove` refuses to remove the current local device. It first performs
a managed key rotation: the target member is tombstoned at the new generation,
the old passphrase and Recovery Key stop unlocking the bundle, and the target
receives no grant for the new content key. Only after the rotated keyfile is
published does it delete the target's device record and valid session-branch
objects. The keyfile and objects owned by another active device remain untouched.
Cleanup is explicit administrative maintenance and is allowed even when the
local synchronization mode is push-only or disabled.

`device rotate-key` performs the same passphrase, Recovery Key and content-key
generation change without tombstoning a member. Every active device receives
a grant for the new generation; retained historical grants let compliant
devices read data published before rotation.

This is cryptographic forward revocation, not data recall. A removed device may
still read plaintext or old keys it copied before removal. A dumb backend also
cannot stop anyone holding its storage credentials from deleting or replacing
objects; backend ACLs or credential rotation are outside this client contract.

An invitation confirms pairing and namespace consistency. In a managed domain,
init also checks the issuer's active membership and current generation before
registering the new device grant.

## 4. Output and failure behavior

Text list output is an aggregate report:

```text
scope: remote
devices: 2
- id=abc123 name=desktop system=windows last-active=2026-08-15T02:00:00Z local
- id=def456 name=unknown system=unknown last-active=unknown
```

JSON contains `scope` and a sorted `devices` array. Each item contains
`id`, `name`, `system`, `lastActiveAt` when known, and `local`.
Device names and IDs are intentionally visible to the operator; passphrases,
backend credentials, local paths, and session content are not emitted.

Rename persists the local name atomically before the best-effort remote
publication. A remote publication error does not roll back the local name and
includes the repair action in its message.

Rotation reports the new generation after the Recovery Key has been confirmed
as saved. The Recovery Key is printed only to the interactive prompt and is not
included in machine-readable output. Removal additionally reports the number of
deleted objects. Deletion is idempotent and sorted, but object stores do not
provide a transaction: if a later delete fails, the error includes the number
already removed. A cleanup failure never rolls back cryptographic revocation; a repeated `device remove --yes DEVICE_ID` only retries cleanup for a member already tombstoned at the current generation.

## 5. Test plan

Tests cover encrypted record round trips, exact-key authentication, bounded and
strict record parsing, legacy branch discovery, deterministic device merging,
option parsing, JSON/text output, rename persistence, current-device removal
protection, managed key rotation, revoked-device rejection, confirmation handling,
partial cleanup reporting, and the absence of session-body reads during device
listing.
