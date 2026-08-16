# In-memory Remote example

This directory is a complete, executable implementation of the
internal/remote.Remote contract. It is intended for adapter experiments,
contract tests, and new backend prototypes.

The store is process-local and loses all objects on restart. It is not a
production backend. A persistent implementation must preserve the same
semantics:

- keep object values opaque and never inspect encrypted bytes;
- map a missing object to remote.ErrNotFound;
- make Delete idempotent;
- verify the declared Put size;
- check context cancellation on every operation;
- treat List ordering and ModTime as advisory;
- reject unsafe keys before reaching a filesystem or object-store API.

A real backend should also implement remote.Prober when it can verify
connectivity and permissions during agentsync init. The existing dir and S3
implementations are the production-oriented references.
