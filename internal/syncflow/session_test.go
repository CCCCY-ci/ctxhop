package syncflow

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestCanonicalizeSessionProducesIndependentCanonicalRecords(t *testing.T) {
	data := adapter.SessionData{
		Records: [][]byte{
			[]byte(`{"cwd":"D:\\Source\\Project","file_path":"D:\\Source\\Project\\src\\main.go","message":{"content":"keep this"}}`),
			[]byte(`{"realParentDir":"D:\\Source\\Agent\\backups","n":9007199254740993}`),
		},
		DroppedTail: true,
	}
	space := adapter.PathSpace{
		ProjectRoot: `D:\Source\Project`,
		AgentHome:   `D:\Source\Agent`,
	}
	installation := adapter.Installation{
		Compatibility:       adapter.CompatFull,
		CompatibilityReason: "agent version is verified",
	}

	stream, err := CanonicalizeSession(data, space, installation)
	if err != nil {
		t.Fatalf("CanonicalizeSession: %v", err)
	}
	if !stream.DroppedTail {
		t.Fatal("DroppedTail = false, want true")
	}
	if stream.Compatibility != adapter.CompatFull || stream.CompatibilityReason != installation.CompatibilityReason {
		t.Fatalf("compatibility = %v, %q", stream.Compatibility, stream.CompatibilityReason)
	}
	if len(stream.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(stream.Records))
	}
	if !bytes.Contains(stream.Records[0], []byte(adapter.TokenProject)) || !bytes.Contains(stream.Records[1], []byte(adapter.TokenAgentHome)) {
		t.Fatalf("canonical records do not contain expected path tokens: %q", stream.Records)
	}
	if bytes.Contains(stream.Records[0], []byte(`D:\Source`)) || bytes.Contains(stream.Records[1], []byte(`D:\Source`)) {
		t.Fatalf("canonical records contain a local path: %q", stream.Records)
	}
	if !bytes.Contains(stream.Records[1], []byte(`9007199254740993`)) {
		t.Fatalf("canonical record changed the large number: %q", stream.Records[1])
	}

	data.Records[0][10] = 'x'
	if bytes.Contains(stream.Records[0], []byte{'x'}) {
		t.Fatal("canonical stream retained caller-owned record storage")
	}
}

func TestCanonicalizeSessionRejectsUnsafeSnapshotsAndPathSchemas(t *testing.T) {
	space := adapter.PathSpace{ProjectRoot: `D:\Source\Project`, AgentHome: `D:\Source\Agent`}
	full := adapter.Installation{Compatibility: adapter.CompatFull}

	cases := []struct {
		name  string
		data  adapter.SessionData
		space adapter.PathSpace
		inst  adapter.Installation
		want  error
	}{
		{
			name:  "skipped record",
			data:  adapter.SessionData{Records: [][]byte{[]byte(`{"ok":true}`)}, Skipped: 1},
			space: space,
			inst:  full,
			want:  ErrInvalidSessionSnapshot,
		},
		{
			name:  "unknown path-keyed container",
			data:  adapter.SessionData{Records: [][]byte{[]byte(`{"unknownContainer":{"D:\\Source\\Project\\secret":true}}`)}},
			space: space,
			inst:  full,
			want:  ErrSessionNotPushable,
		},
		{
			name:  "stopped compatibility",
			data:  adapter.SessionData{Records: [][]byte{[]byte(`{"ok":true}`)}},
			space: space,
			inst:  adapter.Installation{Compatibility: adapter.CompatStopped, CompatibilityReason: "adapter schema is not understood"},
			want:  ErrSessionNotPushable,
		},
		{
			name:  "invalid json",
			data:  adapter.SessionData{Records: [][]byte{[]byte(`{"ok":`)}},
			space: space,
			inst:  full,
			want:  ErrInvalidSessionSnapshot,
		},
		{
			name:  "missing project root",
			data:  adapter.SessionData{Records: [][]byte{[]byte(`{"ok":true}`)}},
			space: adapter.PathSpace{AgentHome: space.AgentHome},
			inst:  full,
			want:  ErrInvalidPathSpace,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CanonicalizeSession(tc.data, tc.space, tc.inst)
			if err == nil || !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), `D:\Source`) {
				t.Fatalf("error leaked a local path: %v", err)
			}
		})
	}
}

func TestCanonicalizeSessionAllowsLimitedCompatibilityAndPreservesFindingSafety(t *testing.T) {
	space := adapter.PathSpace{ProjectRoot: `/source/project`, AgentHome: `/source/agent`}
	installation := adapter.Installation{
		Compatibility:       adapter.CompatLimited,
		CompatibilityReason: "agent version has not been verified",
	}
	stream, err := CanonicalizeSession(adapter.SessionData{Records: [][]byte{[]byte(`{"cwd":"/source/project"}`)}}, space, installation)
	if err != nil {
		t.Fatalf("CanonicalizeSession: %v", err)
	}
	if stream.Compatibility != adapter.CompatLimited || stream.CompatibilityReason != installation.CompatibilityReason {
		t.Fatalf("compatibility = %v, %q", stream.Compatibility, stream.CompatibilityReason)
	}
	if len(stream.UnknownPathFields) != 0 {
		t.Fatalf("unknown path fields = %v", stream.UnknownPathFields)
	}
}

func TestCanonicalStreamPushesThroughExecutorAndPersistsCursor(t *testing.T) {
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
	layout, err := syncer.NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err := syncer.NewCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	initial := syncer.NewPushCursor()
	if err := state.Save(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	options := syncer.DefaultPlanOptions()
	options.MaxRecords = 1
	executor, err := syncer.NewAppendExecutor(store, public, layout, state, options)
	if err != nil {
		t.Fatal(err)
	}

	data := adapter.SessionData{Records: [][]byte{
		[]byte(`{"cwd":"D:\\Source\\Project","message":{"content":"one"}}`),
		[]byte(`{"cwd":"D:\\Source\\Project","message":{"content":"two"}}`),
	}}
	stream, err := CanonicalizeSession(data, adapter.PathSpace{
		ProjectRoot: `D:\Source\Project`,
		AgentHome:   `D:\Source\Agent`,
	}, adapter.Installation{Compatibility: adapter.CompatFull})
	if err != nil {
		t.Fatalf("CanonicalizeSession: %v", err)
	}
	next, err := stream.Push(context.Background(), executor, initial)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	loaded, err := state.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded != next || next.RecordCount != 2 {
		t.Fatalf("loaded cursor = %+v, next = %+v", loaded, next)
	}

	branches, err := syncer.FetchBranches(context.Background(), store, "project", "session", private)
	if err != nil {
		t.Fatalf("FetchBranches: %v", err)
	}
	if len(branches) != 1 || len(branches[0].Records) != len(stream.Records) {
		t.Fatalf("branches = %+v", branches)
	}
	for i := range stream.Records {
		if !bytes.Equal(branches[0].Records[i], stream.Records[i]) {
			t.Fatalf("remote record %d = %q, want %q", i, branches[0].Records[i], stream.Records[i])
		}
	}
}

func TestCanonicalStreamPushChecksContextBeforeRemote(t *testing.T) {
	stream, err := CanonicalizeSession(
		adapter.SessionData{Records: [][]byte{[]byte(`{"ok":true}`)}},
		adapter.PathSpace{ProjectRoot: `/source/project`, AgentHome: `/source/agent`},
		adapter.Installation{Compatibility: adapter.CompatFull},
	)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stream.Push(cancelled, syncer.AppendExecutor{}, syncer.NewPushCursor()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Push error = %v, want context.Canceled", err)
	}
	if _, err := stream.Push(nil, syncer.AppendExecutor{}, syncer.NewPushCursor()); err == nil {
		t.Fatal("Push accepted a nil context")
	}
}
