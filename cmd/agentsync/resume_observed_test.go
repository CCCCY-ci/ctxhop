package main

import (
	"context"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

func TestSaveResumeObservedTipsFiltersSelectedForeignDevices(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	projectID := "project1"
	sessionID := "session1"
	localDeviceID := "local1"
	foreignDeviceID := "foreign1"
	otherDeviceID := "other1"

	localMetadata, err := syncer.NewMetadata(1, syncer.EmptyDigest(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	foreignDigest := [32]byte{1}
	foreignMetadata, err := syncer.NewMetadata(3, foreignDigest, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	otherMetadata, err := syncer.NewMetadata(4, [32]byte{2}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	refs := []syncer.MetadataRef{
		{DeviceID: localDeviceID, Metadata: localMetadata},
		{DeviceID: foreignDeviceID, Metadata: foreignMetadata},
		{DeviceID: otherDeviceID, Metadata: otherMetadata},
	}

	if err := saveResumeObservedTips(ctx, root, projectID, sessionID, localDeviceID, refs, []string{localDeviceID, foreignDeviceID}); err != nil {
		t.Fatal(err)
	}
	layout, err := syncer.NewObjectLayout(projectID, sessionID, localDeviceID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := syncer.NewPullTipStore(root, layout)
	if err != nil {
		t.Fatal(err)
	}
	tips, err := syncflow.LoadObservedTips(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(tips) != 1 || tips[0].DeviceID != foreignDeviceID || tips[0].RecordCount != 3 || tips[0].HeadDigest != foreignDigest {
		t.Fatalf("observed tips = %+v", tips)
	}
}
