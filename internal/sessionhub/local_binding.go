package sessionhub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

// LocalBindingFileName is the local-only state file for one restored or
// materialized NativeSession. It lives below the v2 Session namespace but is
// never uploaded to the remote backend.
const LocalBindingFileName = "binding.json"

var ErrLocalBindingNotFound = errors.New("sessionhub: local binding does not exist")

// LocalBindingPath returns the local state path for a binding. The plaintext
// native session ID is deliberately not used as a path component; the
// Replica ID and Agent already identify the source lineage while keeping
// native identifiers out of directory listings and diagnostics.
func LocalBindingPath(root string, binding LocalBinding) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("sessionhub: local binding root is required")
	}
	if err := binding.Validate(); err != nil {
		return "", err
	}
	return filepath.Join(
		root,
		"state",
		"v2",
		"hubs",
		binding.HubID,
		"projects",
		binding.ProjectID,
		"sessions",
		binding.SessionID,
		"bindings",
		binding.ReplicaID,
		binding.Agent,
		LocalBindingFileName,
	), nil
}

// SaveLocalBinding atomically persists one local-only binding. It never
// touches a NativeSession file or a Remote store.
func SaveLocalBinding(root string, binding LocalBinding) error {
	path, err := LocalBindingPath(root, binding)
	if err != nil {
		return err
	}
	data, err := binding.MarshalBinary()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("sessionhub: create local binding directory: %w", err)
	}
	if err := atomicfile.WriteBytes(path, data); err != nil {
		return fmt.Errorf("sessionhub: write local binding: %w", err)
	}
	return nil
}

// LoadLocalBinding reads and validates one local-only binding by its opaque
// v2 identity tuple.
func LoadLocalBinding(root, hubID, projectID, sessionID, replicaID, agent string) (LocalBinding, error) {
	path, err := localBindingLookupPath(root, hubID, projectID, sessionID, replicaID, agent)
	if err != nil {
		return LocalBinding{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return LocalBinding{}, ErrLocalBindingNotFound
	}
	if err != nil {
		return LocalBinding{}, fmt.Errorf("sessionhub: read local binding: %w", err)
	}
	binding, err := ParseLocalBinding(data)
	if err != nil {
		return LocalBinding{}, err
	}
	if binding.HubID != hubID || binding.ProjectID != projectID || binding.SessionID != sessionID || binding.ReplicaID != replicaID || binding.Agent != agent {
		return LocalBinding{}, fmt.Errorf("%w: binding identity does not match its path", ErrInvalidModel)
	}
	return binding, nil
}

func localBindingLookupPath(root, hubID, projectID, sessionID, replicaID, agent string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("sessionhub: local binding root is required")
	}
	for name, value := range map[string]string{
		"hub": hubID, "project": projectID, "session": sessionID, "replica": replicaID,
	} {
		if err := validateOpaqueID(value); err != nil {
			return "", fmt.Errorf("%w: %s id", ErrInvalidIdentity, name)
		}
	}
	if err := validateAgent(agent); err != nil {
		return "", fmt.Errorf("%w: agent", ErrInvalidIdentity)
	}
	return filepath.Join(root, "state", "v2", "hubs", hubID, "projects", projectID, "sessions", sessionID, "bindings", replicaID, agent, LocalBindingFileName), nil
}
