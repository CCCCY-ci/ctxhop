package sessionhub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

// MaterializeTransactionVersion is the version of the local-only apply
// journal. The journal contains identity, digest and state metadata only; it
// never stores source or target transcript records.
const MaterializeTransactionVersion = 1

// MaterializeTransactionState is the monotonic local apply state machine.
type MaterializeTransactionState string

const (
	MaterializeTransactionPrepared       MaterializeTransactionState = "prepared"
	MaterializeTransactionTargetVerified MaterializeTransactionState = "target-verified"
	MaterializeTransactionCommitted      MaterializeTransactionState = "committed"
)

var (
	ErrMaterializeTransactionNotFound = errors.New("sessionhub: materialize transaction does not exist")
	ErrMaterializeTransactionConflict = errors.New("sessionhub: materialize transaction conflicts with existing state")
)

// MaterializeTransaction is a local recovery record for one deterministic
// materialization request. Its path remains below Hub → Project → Session, so
// transactions from unrelated projects are never mixed into one global list.
// The transaction ID is the request digest; together with PreviewDigest it
// makes retries fail closed when the selected source snapshot or command scope
// changes.
type MaterializeTransaction struct {
	Version             int                         `json:"version"`
	TransactionID       string                      `json:"transactionId"`
	HubID               string                      `json:"hubId"`
	ProjectID           string                      `json:"projectId"`
	SessionID           string                      `json:"sessionId"`
	TargetAgent         string                      `json:"targetAgent"`
	TargetNativeID      string                      `json:"targetNativeId"`
	ReplicaID           string                      `json:"replicaId"`
	ContextPolicy       string                      `json:"contextPolicy"`
	SourceAgent         string                      `json:"sourceAgent,omitempty"`
	SelectedHeads       []string                    `json:"selectedHeads"`
	PreviewDigest       string                      `json:"previewDigest"`
	SelectedRecordCount uint64                      `json:"selectedRecordCount"`
	TargetRecordCount   uint64                      `json:"targetRecordCount"`
	State               MaterializeTransactionState `json:"state"`
	CreatedAt           time.Time                   `json:"createdAt"`
	UpdatedAt           time.Time                   `json:"updatedAt"`
}

// Validate checks a transaction before it crosses the local persistence
// boundary. It deliberately validates only metadata and digests; transcript
// bytes stay in the Agent's native file and in the encrypted Remote Replica.
func (t MaterializeTransaction) Validate() error {
	if t.Version != MaterializeTransactionVersion {
		if t.Version > MaterializeTransactionVersion {
			return fmt.Errorf("%w: materialize transaction version %d", ErrUnsupportedVersion, t.Version)
		}
		return fmt.Errorf("%w: materialize transaction version %d", ErrInvalidModel, t.Version)
	}
	for name, value := range map[string]string{
		"transaction": t.TransactionID,
		"preview":     t.PreviewDigest,
		"hub":         t.HubID,
		"project":     t.ProjectID,
		"session":     t.SessionID,
		"replica":     t.ReplicaID,
	} {
		if name == "preview" {
			if err := validateDigest(value); err != nil {
				return fmt.Errorf("%w: %s digest", ErrInvalidModel, name)
			}
			continue
		}
		if err := validateOpaqueID(value); err != nil {
			return fmt.Errorf("%w: %s id", ErrInvalidIdentity, name)
		}
	}
	if err := validateAgent(t.TargetAgent); err != nil {
		return fmt.Errorf("%w: target Agent", ErrInvalidIdentity)
	}
	if err := validateNativeSessionID(t.TargetNativeID); err != nil {
		return fmt.Errorf("%w: target native session id", ErrInvalidIdentity)
	}
	if err := validateText(t.ContextPolicy, maxNameLength, true); err != nil {
		return fmt.Errorf("%w: context policy", ErrInvalidModel)
	}
	if t.SourceAgent != "" {
		if err := validateAgent(t.SourceAgent); err != nil {
			return fmt.Errorf("%w: source Agent", ErrInvalidIdentity)
		}
	}
	if err := validateIDList(t.SelectedHeads, maxParents, true); err != nil {
		return fmt.Errorf("%w: selected heads", ErrInvalidModel)
	}
	if t.SelectedRecordCount == 0 || t.TargetRecordCount == 0 {
		return fmt.Errorf("%w: record counts must be non-zero", ErrInvalidModel)
	}
	switch t.State {
	case MaterializeTransactionPrepared, MaterializeTransactionTargetVerified, MaterializeTransactionCommitted:
	default:
		return fmt.Errorf("%w: materialize transaction state", ErrInvalidModel)
	}
	if err := validateTime(t.CreatedAt); err != nil {
		return fmt.Errorf("%w: transaction creation time", ErrInvalidModel)
	}
	if err := validateTime(t.UpdatedAt); err != nil {
		return fmt.Errorf("%w: transaction update time", ErrInvalidModel)
	}
	if t.UpdatedAt.Before(t.CreatedAt) {
		return fmt.Errorf("%w: transaction update time precedes creation time", ErrInvalidModel)
	}
	return nil
}

// Advance returns a detached transaction at the next monotonic state. A
// repeated state is allowed for crash recovery; a backward transition is not.
func (t MaterializeTransaction) Advance(state MaterializeTransactionState, now time.Time) (MaterializeTransaction, error) {
	if err := t.Validate(); err != nil {
		return MaterializeTransaction{}, err
	}
	if state != MaterializeTransactionPrepared && state != MaterializeTransactionTargetVerified && state != MaterializeTransactionCommitted {
		return MaterializeTransaction{}, fmt.Errorf("%w: materialize transaction state", ErrInvalidModel)
	}
	if materializeTransactionStateRank(state) < materializeTransactionStateRank(t.State) {
		return MaterializeTransaction{}, fmt.Errorf("%w: materialize transaction cannot move backward", ErrInvalidModel)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	copyValue := t
	copyValue.SelectedHeads = append([]string(nil), t.SelectedHeads...)
	copyValue.State = state
	copyValue.UpdatedAt = now.UTC().Round(0)
	if err := copyValue.Validate(); err != nil {
		return MaterializeTransaction{}, err
	}
	return copyValue, nil
}

func materializeTransactionStateRank(state MaterializeTransactionState) int {
	switch state {
	case MaterializeTransactionPrepared:
		return 1
	case MaterializeTransactionTargetVerified:
		return 2
	case MaterializeTransactionCommitted:
		return 3
	default:
		return 0
	}
}

// MaterializeTransactionPath returns the local journal path for one Session.
// Only opaque Hub/Project/Session/transaction IDs become path components.
func MaterializeTransactionPath(root string, transaction MaterializeTransaction) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("sessionhub: materialize transaction root is required")
	}
	if err := transaction.Validate(); err != nil {
		return "", err
	}
	return filepath.Join(root, "state", "v2", "hubs", transaction.HubID, "projects", transaction.ProjectID, "sessions", transaction.SessionID, "transactions", transaction.TransactionID+".json"), nil
}

// SaveMaterializeTransaction atomically persists one local recovery record.
func SaveMaterializeTransaction(root string, transaction MaterializeTransaction) error {
	path, err := MaterializeTransactionPath(root, transaction)
	if err != nil {
		return err
	}
	data, err := marshalCompact(transactionWithSortedHeads(transaction), "materialize transaction", maxDescriptorBytes)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("sessionhub: create materialize transaction directory: %w", err)
	}
	if existing, err := os.ReadFile(path); err == nil {
		var current MaterializeTransaction
		if err := decodeCompact(existing, &current, "materialize transaction", maxDescriptorBytes); err != nil {
			return err
		}
		if err := current.Validate(); err != nil {
			return err
		}
		if !sameMaterializeTransactionPlan(current, transaction) {
			return ErrMaterializeTransactionConflict
		}
		if materializeTransactionStateRank(current.State) > materializeTransactionStateRank(transaction.State) || (current.State == transaction.State && current.UpdatedAt.After(transaction.UpdatedAt)) {
			// A later recovery attempt must never move a journal backwards.
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sessionhub: inspect materialize transaction: %w", err)
	}
	if err := atomicfile.WriteBytes(path, data); err != nil {
		return fmt.Errorf("sessionhub: write materialize transaction: %w", err)
	}
	return nil
}

// LoadMaterializeTransaction reads and validates one local recovery record.
func LoadMaterializeTransaction(root, hubID, projectID, sessionID, transactionID string) (MaterializeTransaction, error) {
	path, err := materializeTransactionLookupPath(root, hubID, projectID, sessionID, transactionID)
	if err != nil {
		return MaterializeTransaction{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return MaterializeTransaction{}, ErrMaterializeTransactionNotFound
	}
	if err != nil {
		return MaterializeTransaction{}, fmt.Errorf("sessionhub: read materialize transaction: %w", err)
	}
	var transaction MaterializeTransaction
	if err := decodeCompact(data, &transaction, "materialize transaction", maxDescriptorBytes); err != nil {
		return MaterializeTransaction{}, err
	}
	if err := transaction.Validate(); err != nil {
		return MaterializeTransaction{}, err
	}
	if transaction.HubID != hubID || transaction.ProjectID != projectID || transaction.SessionID != sessionID || transaction.TransactionID != transactionID {
		return MaterializeTransaction{}, fmt.Errorf("%w: transaction identity does not match its path", ErrInvalidModel)
	}
	return transactionWithSortedHeads(transaction), nil
}

func materializeTransactionLookupPath(root, hubID, projectID, sessionID, transactionID string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("sessionhub: materialize transaction root is required")
	}
	for name, value := range map[string]string{
		"hub": hubID, "project": projectID, "session": sessionID, "transaction": transactionID,
	} {
		if err := validateOpaqueID(value); err != nil {
			return "", fmt.Errorf("%w: %s id", ErrInvalidIdentity, name)
		}
	}
	return filepath.Join(root, "state", "v2", "hubs", hubID, "projects", projectID, "sessions", sessionID, "transactions", transactionID+".json"), nil
}

func transactionWithSortedHeads(transaction MaterializeTransaction) MaterializeTransaction {
	transaction.SelectedHeads = append([]string(nil), transaction.SelectedHeads...)
	sort.Strings(transaction.SelectedHeads)
	return transaction
}

func sameMaterializeTransactionPlan(left, right MaterializeTransaction) bool {
	left = transactionWithSortedHeads(left)
	right = transactionWithSortedHeads(right)
	if left.Version != right.Version || left.TransactionID != right.TransactionID || left.HubID != right.HubID || left.ProjectID != right.ProjectID || left.SessionID != right.SessionID || left.TargetAgent != right.TargetAgent || left.TargetNativeID != right.TargetNativeID || left.ReplicaID != right.ReplicaID || left.ContextPolicy != right.ContextPolicy || left.SourceAgent != right.SourceAgent || left.PreviewDigest != right.PreviewDigest || left.SelectedRecordCount != right.SelectedRecordCount || left.TargetRecordCount != right.TargetRecordCount {
		return false
	}
	if len(left.SelectedHeads) != len(right.SelectedHeads) {
		return false
	}
	for index := range left.SelectedHeads {
		if left.SelectedHeads[index] != right.SelectedHeads[index] {
			return false
		}
	}
	return true
}
