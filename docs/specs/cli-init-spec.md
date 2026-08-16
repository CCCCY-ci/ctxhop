# CLI initialization specification

## Scope

`agentsync init` prepares one local configuration directory and one storage
backend. It is the only command that creates the first remote keyfile.

## Rules

1. A valid local `config.json` is never replaced by `init`.
2. The selected backend must pass its read, write, list, and cleanup probe
   before the remote keyfile or local configuration is written.
3. Passphrases and credentials are read from stdin or the credential
   environment. They are never accepted as command-line flags, printed, or
   written to `config.json`.
4. If `v1/keyfile` is absent, init creates it once, displays the generated
   Recovery Key, and requires an explicit `saved` confirmation before
   publishing it.
5. If `v1/keyfile` exists, init unlocks that envelope with the entered
   passphrase and never replaces it.
6. The local identifier key is derived from the unlocked data key and is stored
   only through `config.SaveSecrets`. The device ID is generated and persisted
   after the local secret has been saved.
7. Hook installation is best effort after the configuration is valid. A hook
   failure must not invalidate the local configuration; the command reports
   the failure so `doctor` can explain it later.

8. Init displays a non-secret sync-domain fingerprint derived from the
   normalized Remote namespace and the pinned public identity. When joining an
   existing namespace, `--expect-domain-fingerprint` may require an exact
   fingerprint match; a mismatch stops before local configuration is saved. The accepted fingerprint is persisted in `config.json` as a non-secret binding for later commands.

9. `init --invite PATH` imports the Remote settings from a signed device
   invitation. It requires an existing remote keyfile and verifies the namespace,
   domain fingerprint, and proof before saving local secrets or configuration.
   The invitation contains no credentials, passphrase, or session content.

## Failure boundary

No remote keyfile is published when backend probing, passphrase confirmation,
or key derivation fails. A local write failure may leave an incomplete local
setup, but it never rewrites an existing valid configuration or keyfile.
