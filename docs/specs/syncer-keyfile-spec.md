# Syncer keyfile transport specification

## Scope

This specification defines the small transport boundary for the storage
envelope at `v1/keyfile`. It does not derive keys, prompt for secrets, or
unlock the envelope.

The fixed object key is shared by the legacy v1 passphrase-only envelope and
the managed v2 envelope. A v2 envelope contains public generation/member
metadata and encrypted per-device grants; the passphrase and Recovery Key wrap
the bundle of retained content-key epochs. The transport treats those fields as
opaque validated bytes and does not decide membership or revocation.

## Rules

1. `PublishKeyfile` serializes a validated `crypto.Keyfile` and creates
   `v1/keyfile` only when the object is absent.
2. An existing keyfile is never replaced through this API. Replacing it could
   make every existing encrypted session unreadable.
3. `FetchKeyfile` reads at most 1 MiB, parses the complete envelope, and
   returns `ErrNoRemoteKeyfile` only for a genuine missing object.
4. A malformed, oversized, or transport-failed object is an error. It is never
   treated as an uninitialised backend.
5. Both operations require a non-nil context and remote store and honour
   cancellation before publishing or after reading.

## Managed v2 lifecycle

1. `ReplaceKeyfile` is used only after a complete local key rotation or device
   enrollment has been validated. The operation writes the final serialized
   envelope; callers must not publish a partially migrated intermediate state.
2. A device removal rotates the content-key generation and then performs best-
   effort branch cleanup through the ordinary remote object API. A cleanup
   failure never changes the transport meaning of a successfully published
   keyfile.
3. The 1 MiB bound includes the public member table, retained epoch metadata and
   encrypted grants. The crypto layer enforces smaller member/epoch counts and
   strict grant sizes before serialization.

## Error boundary

`ErrRemoteKeyfileExists` distinguishes an attempt to initialise an already
initialised backend. `ErrNoRemoteKeyfile` tells a new device to perform the
first-time setup flow. `remote.ErrNotFound` must not escape as the missing
keyfile signal because the syncer owns the user-facing distinction.
