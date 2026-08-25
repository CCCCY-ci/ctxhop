package syncflow

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestFetchRestorePlanReadsResolvesAndLocalizes(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cleanupRestoreRemoteRoot(t, store.Root)
	layout, err := syncer.NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	records := [][]byte{
		[]byte(`{"cwd":"${AS_PROJECT}","file_path":"${AS_PROJECT}/cmd/main.go"}`),
		[]byte(`{"realParentDir":"${AS_AGENT_HOME}/backups"}`),
	}
	putRestoreShard(t, store, layout, public, 1, 0, syncer.EmptyDigest(), records[:1])
	prefix, err := syncer.DigestRecords(records[:1])
	if err != nil {
		t.Fatal(err)
	}
	putRestoreShard(t, store, layout, public, 2, 1, prefix, records[1:])

	plan, err := FetchRestorePlan(
		context.Background(),
		store,
		"project",
		"session",
		private,
		adapter.PathSpace{ProjectRoot: `D:\Target\Project`, AgentHome: `D:\Target\Agent`},
		adapter.Installation{Compatibility: adapter.CompatFull, CompatibilityReason: "compatibility is determined from session path fields; agent version is informational"},
		RestoreOptions{},
	)
	if err != nil {
		t.Fatalf("FetchRestorePlan: %v", err)
	}
	if plan.ResolutionKind != syncer.ResolutionConsistent || plan.VersionIndex != 0 {
		t.Fatalf("resolution metadata = kind %v, index %d", plan.ResolutionKind, plan.VersionIndex)
	}
	if len(plan.CanonicalRecords) != 2 || len(plan.LocalizedRecords) != 2 {
		t.Fatalf("record counts = %d and %d", len(plan.CanonicalRecords), len(plan.LocalizedRecords))
	}
	if !bytes.Equal(plan.CanonicalRecords[0], records[0]) {
		t.Fatalf("canonical record = %q, want %q", plan.CanonicalRecords[0], records[0])
	}
	want, err := adapter.Localize(records[0], adapter.PathSpace{ProjectRoot: `D:\Target\Project`, AgentHome: `D:\Target\Agent`})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plan.LocalizedRecords[0], want) {
		t.Fatalf("localized record = %q, want %q", plan.LocalizedRecords[0], want)
	}
	if plan.Devices[0] != "device" || plan.HeadDigest != mustDigest(t, records) {
		t.Fatalf("source metadata = devices %v, digest %x", plan.Devices, plan.HeadDigest)
	}
}

func cleanupRestoreRemoteRoot(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		var lastErr error
		for attempt := 0; attempt < 20; attempt++ {
			lastErr = os.RemoveAll(root)
			if lastErr == nil || errors.Is(lastErr, os.ErrNotExist) {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Errorf("remove restore test remote root: %v", lastErr)
	})
}

func TestFetchRestorePlanRequiresAndHonorsForkSelection(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for device, value := range map[string]string{"devicea": `{"n":1}`, "deviceb": `{"n":2}`} {
		layout, err := syncer.NewObjectLayout("project", "session", device)
		if err != nil {
			t.Fatal(err)
		}
		putRestoreShard(t, store, layout, public, 1, 0, syncer.EmptyDigest(), [][]byte{[]byte(value)})
	}
	space := adapter.PathSpace{ProjectRoot: "/target/project", AgentHome: "/target/agent"}
	installation := adapter.Installation{Compatibility: adapter.CompatFull}
	_, err = FetchRestorePlan(context.Background(), store, "project", "session", private, space, installation, RestoreOptions{})
	if err == nil || !errors.Is(err, ErrForkSelectionRequired) {
		t.Fatalf("unselected fork error = %v, want ErrForkSelectionRequired", err)
	}

	selected := 1
	plan, err := FetchRestorePlan(context.Background(), store, "project", "session", private, space, installation, RestoreOptions{VersionIndex: &selected})
	if err != nil {
		t.Fatalf("selected fork: %v", err)
	}
	if plan.ResolutionKind != syncer.ResolutionFork || plan.VersionIndex != selected || string(plan.CanonicalRecords[0]) != `{"n":2}` {
		t.Fatalf("selected plan = kind %v, index %d, records %q", plan.ResolutionKind, plan.VersionIndex, plan.CanonicalRecords)
	}
}

func TestPlanRestoreEnforcesCompatibilityBeforeResolution(t *testing.T) {
	records := [][]byte{[]byte(`{"ok":true}`)}
	resolution := restoreTestResolution(t, syncer.Branch{
		DeviceID:   "device",
		Records:    records,
		HeadDigest: mustDigest(t, records),
	})
	space := adapter.PathSpace{ProjectRoot: "/target/project", AgentHome: "/target/agent"}

	for name, installation := range map[string]adapter.Installation{
		"limited": {Compatibility: adapter.CompatLimited, CompatibilityReason: "unverified"},
		"stopped": {Compatibility: adapter.CompatStopped, CompatibilityReason: "schema is not understood"},
		"unknown": {},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := PlanRestore(resolution, space, installation, RestoreOptions{})
			if err == nil || !errors.Is(err, ErrRestoreCompatibility) {
				t.Fatalf("error = %v, want ErrRestoreCompatibility", err)
			}
		})
	}

	limited, err := PlanRestore(resolution, space, adapter.Installation{Compatibility: adapter.CompatLimited}, RestoreOptions{AllowLimited: true})
	if err != nil {
		t.Fatalf("limited restore with consent: %v", err)
	}
	if limited.Compatibility != adapter.CompatLimited {
		t.Fatalf("compatibility = %v, want limited", limited.Compatibility)
	}
}

func TestPlanRestoreLocalizesAndOwnsBuffers(t *testing.T) {
	records := [][]byte{[]byte(`{"cwd":"${AS_PROJECT}/src/main.go"}`)}
	resolution := restoreTestResolution(t, syncer.Branch{
		DeviceID:   "device",
		Records:    records,
		HeadDigest: mustDigest(t, records),
	})
	plan, err := PlanRestore(
		resolution,
		adapter.PathSpace{ProjectRoot: `D:\Target\Project`, AgentHome: `D:\Target\Agent`},
		adapter.Installation{Compatibility: adapter.CompatFull},
		RestoreOptions{},
	)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	resolution.Versions[0].Records[0][0] = 'x'
	if bytes.Equal(plan.CanonicalRecords[0], resolution.Versions[0].Records[0]) {
		t.Fatal("plan retained resolution-owned canonical bytes")
	}
	plan.CanonicalRecords[0][0] = 'x'
	if bytes.Equal(plan.CanonicalRecords[0], plan.LocalizedRecords[0]) {
		t.Fatal("canonical and localized buffers share storage")
	}
}

func TestPlanRestoreRejectsInvalidSpaceSelectionResolutionAndTokens(t *testing.T) {
	validRecords := [][]byte{[]byte(`{"ok":true}`)}
	valid := restoreTestResolution(t, syncer.Branch{
		DeviceID:   "device",
		Records:    validRecords,
		HeadDigest: mustDigest(t, validRecords),
	})
	full := adapter.Installation{Compatibility: adapter.CompatFull}
	if _, err := PlanRestore(valid, adapter.PathSpace{AgentHome: "/target/agent"}, full, RestoreOptions{}); !errors.Is(err, ErrInvalidPathSpace) {
		t.Fatalf("missing project root error = %v", err)
	}

	malformed := [][]byte{[]byte(`{"cwd":"before${AS_PROJECT}"}`)}
	malformedResolution := restoreTestResolution(t, syncer.Branch{
		DeviceID:   "device",
		Records:    malformed,
		HeadDigest: mustDigest(t, malformed),
	})
	if _, err := PlanRestore(malformedResolution, adapter.PathSpace{ProjectRoot: "/target/project", AgentHome: "/target/agent"}, full, RestoreOptions{}); !errors.Is(err, ErrRestoreLocalization) {
		t.Fatalf("malformed token error = %v", err)
	}

	selection := 4
	if _, err := PlanRestore(valid, adapter.PathSpace{ProjectRoot: "/target/project", AgentHome: "/target/agent"}, full, RestoreOptions{VersionIndex: &selection}); !errors.Is(err, ErrInvalidVersionSelection) {
		t.Fatalf("bad selection error = %v", err)
	}

	badDigest := valid
	badDigest.Versions[0].HeadDigest = [32]byte{}
	if _, err := PlanRestore(badDigest, adapter.PathSpace{ProjectRoot: "/target/project", AgentHome: "/target/agent"}, full, RestoreOptions{}); !errors.Is(err, ErrInvalidRestoreResolution) {
		t.Fatalf("bad digest error = %v", err)
	}
}

func TestFetchRestorePlanChecksContextAndPreconditionsBeforeRemote(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := FetchRestorePlan(cancelled, nil, "project", "session", nil, adapter.PathSpace{}, adapter.Installation{}, RestoreOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v, want context.Canceled", err)
	}

	_, err = FetchRestorePlan(context.Background(), nil, "project", "session", nil, adapter.PathSpace{ProjectRoot: "/project", AgentHome: "/agent"}, adapter.Installation{Compatibility: adapter.CompatLimited}, RestoreOptions{})
	if !errors.Is(err, ErrRestoreCompatibility) {
		t.Fatalf("limited precondition error = %v, want ErrRestoreCompatibility", err)
	}
}

func restoreTestResolution(t *testing.T, branches ...syncer.Branch) syncer.Resolution {
	t.Helper()
	resolution, err := syncer.ResolveBranches(branches)
	if err != nil {
		t.Fatalf("ResolveBranches: %v", err)
	}
	return resolution
}

func putRestoreShard(t *testing.T, store remote.Remote, layout syncer.ObjectLayout, public *ecdh.PublicKey, number, base uint64, prefix [32]byte, records [][]byte) {
	t.Helper()
	shard, err := syncer.NewShard(base, prefix, records)
	if err != nil {
		t.Fatalf("NewShard: %v", err)
	}
	key, err := layout.ShardKey(number)
	if err != nil {
		t.Fatalf("ShardKey: %v", err)
	}
	sealed, err := syncer.SealShard(public, key, shard)
	if err != nil {
		t.Fatalf("SealShard: %v", err)
	}
	if err := store.Put(context.Background(), key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	metadata, err := syncer.NewMetadata(shard.Base+shard.Count(), shard.Digest(), []byte(`{"test":true}`))
	if err != nil {
		t.Fatalf("NewMetadata: %v", err)
	}
	metadataKey, err := layout.MetadataKey()
	if err != nil {
		t.Fatalf("MetadataKey: %v", err)
	}
	sealedMetadata, err := syncer.SealMetadata(public, metadataKey, metadata)
	if err != nil {
		t.Fatalf("SealMetadata: %v", err)
	}
	if err := store.Put(context.Background(), metadataKey, bytes.NewReader(sealedMetadata), int64(len(sealedMetadata))); err != nil {
		t.Fatalf("Put metadata: %v", err)
	}
}

func mustDigest(t *testing.T, records [][]byte) [32]byte {
	t.Helper()
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatalf("DigestRecords: %v", err)
	}
	return digest
}
