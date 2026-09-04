package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

func TestBuildSessionListKeepsDifferentAgentsAsDistinctLegacySessions(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	now := time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)
	current := project.Project{Root: t.TempDir(), Identity: project.Identity{Kind: project.KindRemote, Value: "github.com/example/app"}}
	v1ProjectID, err := crypto.ProjectID(key, current.Identity.Value)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := sessionhub.NewDefaultRegistry(key, now)
	if err != nil {
		t.Fatal(err)
	}
	collection := listCollection{
		current:       current,
		identifierKey: key,
		projectID:     v1ProjectID,
		localDeviceID: "device01",
		localSessions: []adapter.SessionRef{
			{Agent: "claude-code", NativeID: "native-claude", Title: "same title", CreatedAt: now, UpdatedAt: now},
			{Agent: "codex", NativeID: "native-codex", Title: "same title", CreatedAt: now, UpdatedAt: now},
		},
	}

	report, err := buildSessionList(collection, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 2 {
		t.Fatalf("sessions = %+v, want two independent logical sessions", report.Sessions)
	}
	if report.Hub.Name != sessionhub.DefaultHubLogicalID || report.Project.ID == "" {
		t.Fatalf("scope = %+v", report)
	}
	if report.Sessions[0].Sources[0].Agent != "claude-code" || report.Sessions[1].Sources[0].Agent != "codex" {
		t.Fatalf("agent sources were not preserved: %+v", report.Sessions)
	}
}

func TestBuildSessionListGroupsOnlyAnExistingRemoteSessionGroup(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	current := project.Project{Root: t.TempDir(), Identity: project.Identity{Kind: project.KindManual, Value: "manual:app"}}
	v1ProjectID, err := crypto.ProjectID(key, current.Identity.Value)
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2026, 8, 27, 5, 0, 0, 0, time.UTC)
	claudePayload, err := syncflow.EncodeSessionSummary(adapter.SessionRef{
		Agent: "claude-code", NativeID: "native-claude", Title: "claude", CreatedAt: updated.Add(-time.Hour), UpdatedAt: updated,
	})
	if err != nil {
		t.Fatal(err)
	}
	codexPayload, err := syncflow.EncodeSessionSummary(adapter.SessionRef{
		Agent: "codex", NativeID: "native-codex", Title: "codex", CreatedAt: updated.Add(-30 * time.Minute), UpdatedAt: updated.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	claudeMetadata, err := syncer.NewMetadata(3, [32]byte{1}, claudePayload)
	if err != nil {
		t.Fatal(err)
	}
	codexMetadata, err := syncer.NewMetadata(5, [32]byte{2}, codexPayload)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := sessionhub.NewDefaultRegistry(key, updated)
	if err != nil {
		t.Fatal(err)
	}
	report, err := buildSessionList(listCollection{
		current:       current,
		identifierKey: key,
		projectID:     v1ProjectID,
		remoteSessions: []syncer.ProjectMetadataRef{{
			SessionID: "legacygroup",
			Devices: []syncer.MetadataRef{
				{DeviceID: "device01", Metadata: claudeMetadata},
				{DeviceID: "device02", Metadata: codexMetadata},
			},
		}},
	}, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 1 || len(report.Sessions[0].Sources) != 2 {
		t.Fatalf("report = %+v, want one group with two sources", report)
	}
	if report.Sessions[0].Sources[0].Agent != "claude-code" || report.Sessions[0].Sources[1].Agent != "codex" {
		t.Fatalf("sources = %+v", report.Sessions[0].Sources)
	}
	if report.Sessions[0].RecordCount != 5 {
		t.Fatalf("record count = %d, want max remote branch count", report.Sessions[0].RecordCount)
	}
}

func TestBuildSessionListProjectsRemoteNativeReplicaMetadata(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	current := project.Project{Root: t.TempDir(), Identity: project.Identity{Kind: project.KindRemote, Value: "github.com/example/app"}}
	v1ProjectID, err := crypto.ProjectID(key, current.Identity.Value)
	if err != nil {
		t.Fatal(err)
	}
	hubScope, projectScope, _, err := sessionHubAndProject(key, current)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 27, 6, 0, 0, 0, time.UTC)
	layout, err := syncer.NewReplicaLayout(hubScope.ID, projectScope.ID, "session01", "replica01", "device02")
	if err != nil {
		t.Fatal(err)
	}
	replica := syncer.ReplicaMetadata{
		Layout: layout,
		Descriptor: sessionhub.NativeReplicaDescriptor{
			Version:   sessionhub.ModelVersion,
			ReplicaID: "replica01",
			SessionID: "session01",
			Source: sessionhub.NativeSource{
				Agent:            "codex",
				NativeSessionKey: "nativekey01",
				NativeSessionID:  "native-remote-session-that-is-recoverable",
				DeviceID:         "device02",
				Generation:       1,
				NativeFormat:     "codex-jsonl",
			},
			Origin:    sessionhub.ReplicaOrigin{Kind: sessionhub.ReplicaOriginNative},
			CreatedAt: now,
		},
		Tip: &sessionhub.ReplicaTip{Version: sessionhub.ModelVersion, ReplicaID: "replica01", RecordCount: 7, ShardCount: 1, LastShard: 1, HeadDigest: strings.Repeat("0", 64), UpdatedAt: now.Add(time.Hour)},
	}
	registry, err := sessionhub.NewDefaultRegistry(key, now)
	if err != nil {
		t.Fatal(err)
	}
	report, err := buildSessionList(listCollection{
		current:        current,
		identifierKey:  key,
		projectID:      v1ProjectID,
		localDeviceID:  "device01",
		remoteReplicas: []syncer.ProjectReplicaMetadataRef{{SessionID: "session01", SessionDescriptor: &sessionhub.SessionDescriptor{SessionID: "session01", ProjectID: projectScope.ID, Title: "remote title", CreatedAt: now, CreatedBy: sessionhub.SessionCreator{Agent: "codex", DeviceID: "device02"}, Version: sessionhub.ModelVersion, Lifecycle: sessionhub.SessionActive}, Replicas: []syncer.ReplicaMetadata{replica}}},
	}, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 1 || report.Sessions[0].SessionID != "session01" || report.Sessions[0].Title != "remote title" {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Sessions[0].Sources) != 1 {
		t.Fatalf("sources = %+v", report.Sessions[0].Sources)
	}
	source := report.Sessions[0].Sources[0]
	if source.Agent != "codex" || source.NativeID != "native-remote-session-that-is-recoverable" || source.NativeSessionKey != "nativekey01" || source.DeviceID != "device02" || !source.Complete || source.RecordCount != 7 {
		t.Fatalf("remote Replica source = %+v", source)
	}
}

func TestBuildSessionListHonoursAnExplicitLocalBinding(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	current := project.Project{Root: t.TempDir(), Identity: project.Identity{Kind: project.KindRemote, Value: "github.com/example/app"}}
	v1ProjectID, err := crypto.ProjectID(key, current.Identity.Value)
	if err != nil {
		t.Fatal(err)
	}
	hubScope, projectScope, _, err := sessionHubAndProject(key, current)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := sessionhub.NewDefaultRegistry(key, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	projectRecord, err := registry.EnsureProject(key, sessionhub.ProjectIdentityRemote, current.Identity.Value, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if projectRecord.Descriptor.ProjectID != projectScope.ID || hubScope.ID == "" {
		t.Fatalf("scope = %+v/%+v", hubScope, projectScope)
	}
	nativeID := "native-bound"
	legacyID, err := crypto.SessionID(key, v1ProjectID, nativeID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := registry.EnsureLegacySession(key, projectScope.ID, "otherlegacy", "target", time.Now().UTC(), sessionhub.SessionCreator{Agent: "codex", DeviceID: "device01"})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.BindNativeSession(projectScope.ID, target.Descriptor.SessionID, sessionhub.NativeSessionBinding{
		Agent:           "codex",
		NativeSessionID: nativeID,
		LegacySessionID: legacyID,
		BoundAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	report, err := buildSessionList(listCollection{
		current:       current,
		identifierKey: key,
		projectID:     v1ProjectID,
		localDeviceID: "device01",
		localSessions: []adapter.SessionRef{{Agent: "codex", NativeID: nativeID, Title: "bound locally"}},
	}, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 1 || report.Sessions[0].SessionID != target.Descriptor.SessionID {
		t.Fatalf("report = %+v, want explicit target session %q", report, target.Descriptor.SessionID)
	}
}

func TestWriteSessionListTextShowsAgentAndDeviceSource(t *testing.T) {
	var output bytes.Buffer
	err := writeSessionListText(&output, sessionListReport{
		Scope:    "project",
		Hub:      sessionHubScope{ID: "hub01", Name: "default"},
		Project:  sessionProjectScope{ID: "project01"},
		Sessions: []sessionListEntry{{SessionID: "session01", Title: "a session", Sources: []sessionSourceEntry{{Agent: "claude-code", NativeID: "native01", DeviceID: "device01", Local: true}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"hub: default (hub01)", "project: project01", "claude-code:native01@local"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output = %q, missing %q", text, want)
		}
	}
}

func TestHumanSessionViewsShortenNativeIDs(t *testing.T) {
	const nativeID = "native-session-identifier-that-should-not-be-printed-in-full"
	report := sessionListReport{
		Scope:   "project",
		Hub:     sessionHubScope{ID: "hub01", Name: "default"},
		Project: sessionProjectScope{ID: "project01"},
		Sessions: []sessionListEntry{{
			SessionID: "session01",
			Sources:   []sessionSourceEntry{{Agent: "codex", NativeID: nativeID, DeviceID: "device01", Local: true}},
		}},
	}

	var listOutput bytes.Buffer
	if err := writeSessionListText(&listOutput, report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listOutput.String(), nativeID) {
		t.Fatalf("list output leaked full native ID: %q", listOutput.String())
	}
	if !strings.Contains(listOutput.String(), "codex:…ted-in-full@local") {
		t.Fatalf("list output did not include the native ID suffix: %q", listOutput.String())
	}

	var showOutput bytes.Buffer
	if err := writeSessionShow(&showOutput, report, "session01", false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(showOutput.String(), nativeID) {
		t.Fatalf("show output leaked full native ID: %q", showOutput.String())
	}
	if !strings.Contains(showOutput.String(), "native=…ted-in-full") {
		t.Fatalf("show output did not include the native ID suffix: %q", showOutput.String())
	}

	var discoverOutput bytes.Buffer
	if err := writeSessionDiscoverText(&discoverOutput, sessionDiscoverReport{
		Scope:          "project",
		Hub:            report.Hub,
		Project:        report.Project,
		NativeSessions: []sessionDiscoverEntry{{Agent: "codex", NativeID: nativeID, State: "unbound"}},
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(discoverOutput.String(), nativeID) {
		t.Fatalf("discover output leaked full native ID: %q", discoverOutput.String())
	}

	var jsonOutput bytes.Buffer
	if err := writeSessionListJSON(&jsonOutput, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOutput.String(), nativeID) {
		t.Fatalf("authorized JSON output lost the full native ID: %q", jsonOutput.String())
	}
}

func TestRegisterPushedSessionsCreatesMetadataOnlyLocalBinding(t *testing.T) {
	configDir := t.TempDir()
	key := []byte(strings.Repeat("k", 32))
	identity := project.Identity{Kind: project.KindManual, Value: "manual:app"}
	if err := registerPushedSessions(configDir, key, "device01", identity, []pushedNativeSession{{
		Agent:           "codex",
		NativeID:        "native-one",
		LegacySessionID: "legacyone",
		Title:           "pushed title",
		CreatedAt:       time.Date(2026, 8, 27, 6, 0, 0, 0, time.UTC),
	}}); err != nil {
		t.Fatal(err)
	}
	registry, err := sessionhub.LoadRegistry(configDir)
	if err != nil {
		t.Fatal(err)
	}
	_, projectScope, _, err := sessionHubAndProject(key, project.Project{Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	found, ok := registry.FindSessionByNative(projectScope.ID, "codex", "native-one", "legacyone")
	if !ok {
		t.Fatal("successful push was not registered")
	}
	if found.Descriptor.Title != "pushed title" || len(found.Sources) != 1 {
		t.Fatalf("registered session = %+v", found)
	}
}

func TestParseSessionOptions(t *testing.T) {
	list, err := parseSessionOptions([]string{"list", "--json"})
	if err != nil || list.action != sessionActionList || !list.json {
		t.Fatalf("list options = %+v, err=%v", list, err)
	}
	show, err := parseSessionOptions([]string{"show", "--json", "session01"})
	if err != nil || show.action != sessionActionShow || show.sessionID != "session01" || !show.json {
		t.Fatalf("show options = %+v, err=%v", show, err)
	}
	show, err = parseSessionOptions([]string{"show", "session01", "--json"})
	if err != nil || show.action != sessionActionShow || show.sessionID != "session01" || !show.json {
		t.Fatalf("show positional-first options = %+v, err=%v", show, err)
	}
	if _, err := parseSessionOptions([]string{"show"}); err == nil {
		t.Fatal("show accepted a missing session ID")
	}
	if _, err := parseSessionOptions([]string{"list", "unexpected"}); err == nil {
		t.Fatal("list accepted an unexpected argument")
	}
}
