# Syncer keyfile transport specification

## Scope

This specification defines the small transport boundary for the storage
envelope at `v1/keyfile`. It does not derive keys, prompt for secrets, or
unlock the envelope.

## Rules

1. `PublishKeyfile` serializes a validated `crypto.Keyfile` and creates
   `v1/keyfile` only when the object is absent.
2. An existing keyfile is never replaced through this API. Replacing it could
   make every existing encrypted session unreadable.
3. `FetchKeyfile` reads at most 64 KiB, parses the complete envelope, and
   returns `ErrNoRemoteKeyfile` only for a genuine missing object.
4. A malformed, oversized, or transport-failed object is an error. It is never
   treated as an uninitialised backend.
5. Both operations require a non-nil context and remote store and honour
   cancellation before publishing or after reading.

## Error boundary

`ErrRemoteKeyfileExists` distinguishes an attempt to initialise an already
initialised backend. `ErrNoRemoteKeyfile` tells a new device to perform the
first-time setup flow. `remote.ErrNotFound` must not escape as the missing
keyfile signal because the syncer owns the user-facing distinction.
