package sessionhub

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func testMaterializeTransaction() MaterializeTransaction {
	return MaterializeTransaction{
		Version:             MaterializeTransactionVersion,
		TransactionID:       testDigest('a'),
		HubID:               testOpaque('b'),
		ProjectID:           testOpaque('c'),
		SessionID:           testOpaque('d'),
		TargetAgent:         "codex",
		TargetNativeID:      "ctxhop-target",
		ReplicaID:           testOpaque('e'),
		ContextPolicy:       "causal-head",
		SelectedHeads:       []string{"headb", "heada"},
		PreviewDigest:       testDigest('f'),
		SelectedRecordCount: 4,
		TargetRecordCount:   5,
		State:               MaterializeTransactionPrepared,
		CreatedAt:           testTime(),
		UpdatedAt:           testTime(),
	}
}

func TestMaterializeTransactionRoundTripKeepsSessionScopeAndNoBodies(t *testing.T) {
	root := t.TempDir()
	transaction := testMaterializeTransaction()
	if err := SaveMaterializeTransaction(root, transaction); err != nil {
		t.Fatal(err)
	}
	path, err := MaterializeTransactionPath(root, transaction)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, transaction.TargetNativeID) {
		t.Fatalf("transaction path leaked target native ID: %q", path)
	}
	loaded, err := LoadMaterializeTransaction(root, transaction.HubID, transaction.ProjectID, transaction.SessionID, transaction.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.SelectedHeads) != 2 || loaded.SelectedHeads[0] != "heada" || loaded.State != MaterializeTransactionPrepared {
		t.Fatalf("loaded transaction = %+v", loaded)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "records") || strings.Contains(string(data), "encoded") {
		t.Fatalf("transaction journal appears to contain transcript data: %s", data)
	}
}

func TestMaterializeTransactionAdvanceIsMonotonic(t *testing.T) {
	transaction := testMaterializeTransaction()
	verified, err := transaction.Advance(MaterializeTransactionTargetVerified, transaction.CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	committed, err := verified.Advance(MaterializeTransactionCommitted, transaction.CreatedAt.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if committed.State != MaterializeTransactionCommitted || !committed.UpdatedAt.After(transaction.UpdatedAt) {
		t.Fatalf("committed transaction = %+v", committed)
	}
	if _, err := committed.Advance(MaterializeTransactionPrepared, time.Now().UTC()); !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("backward transition error = %v, want ErrInvalidModel", err)
	}
}

func TestSaveMaterializeTransactionDoesNotMoveDurableStateBackward(t *testing.T) {
	root := t.TempDir()
	transaction := testMaterializeTransaction()
	if err := SaveMaterializeTransaction(root, transaction); err != nil {
		t.Fatal(err)
	}
	verified, err := transaction.Advance(MaterializeTransactionTargetVerified, transaction.CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveMaterializeTransaction(root, verified); err != nil {
		t.Fatal(err)
	}
	if err := SaveMaterializeTransaction(root, transaction); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMaterializeTransaction(root, transaction.HubID, transaction.ProjectID, transaction.SessionID, transaction.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != MaterializeTransactionTargetVerified {
		t.Fatalf("durable state moved backward to %q", loaded.State)
	}

	conflict := verified
	conflict.PreviewDigest = testDigest('0')
	if err := SaveMaterializeTransaction(root, conflict); !errors.Is(err, ErrMaterializeTransactionConflict) {
		t.Fatalf("conflicting save error = %v, want ErrMaterializeTransactionConflict", err)
	}
}

func TestLoadMaterializeTransactionReportsMissingAndRejectsWrongScope(t *testing.T) {
	root := t.TempDir()
	transaction := testMaterializeTransaction()
	if _, err := LoadMaterializeTransaction(root, transaction.HubID, transaction.ProjectID, transaction.SessionID, transaction.TransactionID); !errors.Is(err, ErrMaterializeTransactionNotFound) {
		t.Fatalf("missing transaction error = %v", err)
	}
	if _, err := LoadMaterializeTransaction(root, "../escape", transaction.ProjectID, transaction.SessionID, transaction.TransactionID); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("unsafe path error = %v, want ErrInvalidIdentity", err)
	}
}
