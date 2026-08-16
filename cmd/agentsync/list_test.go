package main

import (
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

func TestMergeListSessionsCombinesLocalAndForeignMetadata(t *testing.T) {
	identifierKey := make([]byte, 32)
	projectID := "projectone"
	localRef := adapter.SessionRef{
		NativeID:  "native-one",
		Title:     "local title",
		CreatedAt: time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC),
	}
	sessionID, err := crypto.SessionID(identifierKey, projectID, localRef.NativeID)
	if err != nil {
		t.Fatal(err)
	}
	foreignPayload, err := encodeListSummary("native-one", "foreign title", time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	remoteOnlyPayload, err := encodeListSummary("native-foreign", "foreign-only title", time.Date(2026, 8, 15, 4, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	report := mergeListSessions("local-device", identifierKey, projectID, []adapter.SessionRef{localRef}, []syncer.ProjectMetadataRef{
		{SessionID: sessionID, Devices: []syncer.MetadataRef{{DeviceID: "foreign-device", Metadata: mustListMetadata(t, foreignPayload)}}},
		{SessionID: "remotesession", Devices: []syncer.MetadataRef{{DeviceID: "other-device", Metadata: mustListMetadata(t, remoteOnlyPayload)}}},
	})
	if len(report.Sessions) != 2 {
		t.Fatalf("sessions = %+v", report.Sessions)
	}
	merged := report.Sessions[1]
	if merged.RemoteID != sessionID || !merged.Local || merged.NativeID != localRef.NativeID || merged.Title != "foreign title" {
		t.Fatalf("merged session = %+v", merged)
	}
	if len(merged.Sources) != 2 || merged.Sources[0] != "device-foreign-device" || merged.Sources[1] != "local" {
		t.Fatalf("merged sources = %+v", merged.Sources)
	}
}

func encodeListSummary(nativeID, title string, updated time.Time) ([]byte, error) {
	return syncflow.EncodeSessionSummary(adapter.SessionRef{
		NativeID:  nativeID,
		Title:     title,
		CreatedAt: updated.Add(-time.Minute),
		UpdatedAt: updated,
	})
}

func mustListMetadata(t *testing.T, payload []byte) syncer.Metadata {
	t.Helper()
	metadata, err := syncer.NewMetadata(1, [32]byte{1}, payload)
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}
