package syncflow

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/CCCCY-ci/ctxhop/internal/environment"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

// ErrMaterializeSnapshotChanged means that the authenticated source graph or
// one of the complete Replica bodies changed during a read-only materialize
// plan. A caller must discard the plan and start a fresh preview.
var ErrMaterializeSnapshotChanged = errors.New("syncflow: materialize source snapshot changed")

// digestMaterializeRemoteSnapshot creates a deterministic digest from the
// authenticated logical graph and the complete selected Replica snapshots.
// It intentionally includes metadata and body digests, but never exposes the
// source records in CLI output or local transaction journals.
func digestMaterializeRemoteSnapshot(graph *sessionhub.Graph, heads []string, replicas map[string]syncer.ReplicaSnapshot, environmentContents []environment.ComponentContent) ([32]byte, error) {
	if graph == nil {
		return [32]byte{}, fmt.Errorf("graph is nil")
	}

	contributionIDs := graph.ContributionIDs()
	contributions := make([][]byte, 0, len(contributionIDs))
	for _, id := range contributionIDs {
		contribution, ok := graph.Contribution(id)
		if !ok {
			return [32]byte{}, fmt.Errorf("Contribution %q is unavailable", id)
		}
		encoded, err := contribution.MarshalBinary()
		if err != nil {
			return [32]byte{}, fmt.Errorf("encode Contribution %q: %w", id, err)
		}
		contributions = append(contributions, encoded)
	}

	ids := make([]string, 0, len(replicas))
	for id := range replicas {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	serializedReplicas := make([]materializeReplicaSnapshotDigest, 0, len(ids))
	for _, id := range ids {
		snapshot := replicas[id]
		prefix, err := snapshot.Layout.ReplicaPrefix()
		if err != nil {
			return [32]byte{}, fmt.Errorf("Replica %q layout: %w", id, err)
		}
		descriptor, err := snapshot.Descriptor.MarshalBinary()
		if err != nil {
			return [32]byte{}, fmt.Errorf("encode Replica %q descriptor: %w", id, err)
		}
		tip, err := snapshot.Tip.MarshalBinary()
		if err != nil {
			return [32]byte{}, fmt.Errorf("encode Replica %q tip: %w", id, err)
		}
		bodyDigest, err := syncer.DigestRecords(snapshot.Records)
		if err != nil {
			return [32]byte{}, fmt.Errorf("digest Replica %q body: %w", id, err)
		}
		serializedReplicas = append(serializedReplicas, materializeReplicaSnapshotDigest{
			ReplicaID:   id,
			Layout:      prefix,
			Descriptor:  descriptor,
			Tip:         tip,
			BodyDigest:  hex.EncodeToString(bodyDigest[:]),
			RecordCount: uint64(len(snapshot.Records)),
		})
	}
	serializedEnvironment := make([][]byte, 0, len(environmentContents))
	for _, content := range environmentContents {
		if err := content.Validate(); err != nil {
			return [32]byte{}, fmt.Errorf("environment component is invalid: %w", err)
		}
		encoded, err := json.Marshal(content)
		if err != nil {
			return [32]byte{}, fmt.Errorf("encode environment component: %w", err)
		}
		serializedEnvironment = append(serializedEnvironment, encoded)
	}

	wire := struct {
		SessionID     string                             `json:"sessionId"`
		Heads         []string                           `json:"heads"`
		Contributions [][]byte                           `json:"contributions"`
		Replicas      []materializeReplicaSnapshotDigest `json:"replicas"`
		Environment   [][]byte                           `json:"environment"`
	}{
		SessionID:     graph.SessionID(),
		Heads:         append([]string(nil), heads...),
		Contributions: contributions,
		Replicas:      serializedReplicas,
		Environment:   serializedEnvironment,
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode source snapshot digest: %w", err)
	}
	return sessionhub.DigestBytes(data), nil
}

type materializeReplicaSnapshotDigest struct {
	ReplicaID   string `json:"replicaId"`
	Layout      string `json:"layout"`
	Descriptor  []byte `json:"descriptor"`
	Tip         []byte `json:"tip"`
	BodyDigest  string `json:"bodyDigest"`
	RecordCount uint64 `json:"recordCount"`
}
