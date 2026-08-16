# Managed device authorization gate

| Field | Value |
|---|---|
| Date | 2026-08-16 |
| Scope | Local directory Remote and synthetic AgentSync CLI configurations |
| Test | `go test ./cmd/agentsync -run TestDeviceRemoveRotatesKeyAndRevokesDevice -count=1` |
| External services | None |

## Covered flow

1. Device A initializes a legacy-compatible domain and creates a signed invite.
2. Device B joins through `init --invite`; enrollment registers B's device public
   key in the managed v2 keyfile.
3. Device A runs `device remove --yes B`, confirms a new passphrase, saves the
   newly printed Recovery Key, and publishes generation 2.
4. The old passphrase and old Recovery Key no longer unlock the keyfile. B's
   device private key cannot unlock the current generation. A can still unlock
   the retained generation-1 history.
5. Device A creates a new invite with the new generation. Device C joins with
   the new passphrase and receives a distinct active device grant.

## Acceptance result

PASS. The test verifies the generation change, revoked-device rejection, old
secret invalidation, stable identifier namespace, historical read compatibility,
and enrollment after rotation. Remote branch cleanup is also executed through
the directory Remote path.

## Boundary

This is cryptographic forward revocation. It cannot recall plaintext or old key
material already copied by B. The directory/S3 Remote contract is intentionally
content-agnostic and does not provide an ACL: an actor that still holds backend
credentials may delete or replace objects. Provider-side ACL or credential
rotation remains a separate deployment control.