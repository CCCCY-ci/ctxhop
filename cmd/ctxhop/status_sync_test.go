package main

import (
	"context"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

func TestCalculateStatusSessionsSeparatesForeignUpdates(t *testing.T) {
	identifierKey := make([]byte, 32)
	projectID := "projectone"
	localDevice := "localdevice"
	foreignDevice := "foreigndevice"
	localRef := adapter.SessionRef{
		NativeID:  "native-one",
		Title:     "session",
		CreatedAt: time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC),
	}
	sessionID, err := crypto.SessionID(identifierKey, projectID, localRef.NativeID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := syncflow.EncodeSessionSummary(localRef)
	if err != nil {
		t.Fatal(err)
	}
	localMetadata, err := syncer.NewMetadata(1, [32]byte{1}, payload)
	if err != nil {
		t.Fatal(err)
	}
	foreignMetadata, err := syncer.NewMetadata(2, [32]byte{2}, payload)
	if err != nil {
		t.Fatal(err)
	}

	stateRoot := t.TempDir()
	layout, err := syncer.NewObjectLayout(projectID, sessionID, localDevice)
	if err != nil {
		t.Fatal(err)
	}
	cursorStore, err := syncer.NewCursorStore(stateRoot, layout)
	if err != nil {
		t.Fatal(err)
	}
	cursor := syncer.NewPushCursor()
	cursor.NextShard = 2
	cursor.RecordCount = 1
	cursor.HeadDigest = [32]byte{1}
	if err := cursorStore.Save(context.Background(), cursor); err != nil {
		t.Fatal(err)
	}

	counts, err := calculateStatusSessions(context.Background(), localDevice, projectID, identifierKey, stateRoot,
		[]adapter.SessionRef{localRef}, []syncer.ProjectMetadataRef{
			{SessionID: sessionID, Devices: []syncer.MetadataRef{
				{DeviceID: localDevice, Metadata: localMetadata},
				{DeviceID: foreignDevice, Metadata: foreignMetadata},
			}},
			{SessionID: "remoteonly", Devices: []syncer.MetadataRef{
				{DeviceID: foreignDevice, Metadata: foreignMetadata},
			}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Local != 1 || counts.Remote != 2 || counts.ForeignUpdates != 1 || counts.RemoteOnly != 1 || counts.Synced != 0 || counts.LocalOnly != 0 || counts.Attention != 0 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestCountStatusQueueClassifiesDueWork(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	key, err := syncer.NewQueueKey("projectone", "sessionone", "localdevice")
	if err != nil {
		t.Fatal(err)
	}
	retryKey, err := syncer.NewQueueKey("projectone", "sessiontwo", "localdevice")
	if err != nil {
		t.Fatal(err)
	}
	futureKey, err := syncer.NewQueueKey("projectone", "sessionthree", "localdevice")
	if err != nil {
		t.Fatal(err)
	}
	blockedKey, err := syncer.NewQueueKey("projectone", "sessionfour", "localdevice")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := syncer.NewQueueKey("otherproject", "sessionfive", "localdevice")
	if err != nil {
		t.Fatal(err)
	}

	counts := countStatusQueue(syncer.QueueSnapshot{Items: []syncer.QueueItem{
		{Key: key, State: syncer.QueuePending},
		{Key: retryKey, Attempt: 1, NextAttemptAt: now.Add(-time.Minute), State: syncer.QueuePending, Failure: syncer.FailureNetwork},
		{Key: futureKey, Attempt: 1, NextAttemptAt: now.Add(time.Minute), State: syncer.QueuePending, Failure: syncer.FailureUnknown},
		{Key: blockedKey, State: syncer.QueueBlocked, Failure: syncer.FailureExcluded},
		{Key: otherKey, State: syncer.QueuePending},
	}}, "projectone", "localdevice", now)
	if counts.Pending != 3 || counts.Due != 2 || counts.RetryScheduled != 1 || counts.Blocked != 1 {
		t.Fatalf("queue counts = %+v", counts)
	}
}

func TestParseStatusOptionsRemote(t *testing.T) {
	options, err := parseStatusOptions([]string{"--remote", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.remote || !options.json {
		t.Fatalf("options = %+v", options)
	}
}
