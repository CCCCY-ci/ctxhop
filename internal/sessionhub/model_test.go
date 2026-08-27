package sessionhub

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testOpaque(ch byte) string {
	return strings.Repeat(string(ch), 26)
}

func testDigest(ch byte) string {
	return strings.Repeat(string(ch), 64)
}

func testTime() time.Time {
	return time.Date(2026, time.August, 27, 8, 10, 0, 123456789, time.UTC)
}

func testHub() HubDescriptor {
	return HubDescriptor{
		Version:   ModelVersion,
		HubID:     testOpaque('a'),
		Name:      "work",
		CreatedAt: testTime(),
		Lifecycle: HubActive,
	}
}

func testProject() ProjectDescriptor {
	return ProjectDescriptor{
		Version:             ModelVersion,
		HubID:               testOpaque('a'),
		ProjectID:           testOpaque('b'),
		IdentityKind:        ProjectIdentityRemote,
		IdentityFingerprint: "hmac256:project-fingerprint",
		CreatedAt:           testTime(),
		Lifecycle:           ProjectActive,
	}
}

func testSession() SessionDescriptor {
	return SessionDescriptor{
		Version:   ModelVersion,
		SessionID: testOpaque('c'),
		ProjectID: testOpaque('b'),
		Title:     "实现 Session Hub",
		CreatedAt: testTime(),
		CreatedBy: SessionCreator{
			Agent:    "claude-code",
			DeviceID: testOpaque('d'),
		},
		Lifecycle: SessionActive,
	}
}

func testReplica() NativeReplicaDescriptor {
	return NativeReplicaDescriptor{
		Version:   ModelVersion,
		ReplicaID: testOpaque('e'),
		SessionID: testOpaque('c'),
		Source: NativeSource{
			Agent:            "claude-code",
			NativeSessionKey: testOpaque('f'),
			DeviceID:         testOpaque('d'),
			Generation:       1,
			NativeFormat:     "claude-jsonl",
			AgentVersion:     "1.2.3",
		},
		Origin: ReplicaOrigin{
			Kind:      ReplicaOriginNative,
			BaseHeads: []string{},
		},
		CreatedAt: testTime(),
	}
}

func testTip() ReplicaTip {
	return ReplicaTip{
		Version:     ModelVersion,
		ReplicaID:   testOpaque('e'),
		RecordCount: 4,
		ShardCount:  1,
		LastShard:   1,
		HeadDigest:  testDigest('0'),
		UpdatedAt:   testTime(),
	}
}

func testRange(replicaID string, start, end uint64) RangeRef {
	return RangeRef{
		ReplicaID:    replicaID,
		StartRecord:  start,
		EndRecord:    end,
		PrefixDigest: testDigest('0'),
		RangeDigest:  testDigest('1'),
	}
}

func testContribution() Contribution {
	return Contribution{
		Version:        ModelVersion,
		ContributionID: testOpaque('g'),
		SessionID:      testOpaque('c'),
		Source: ContributionSource{
			Agent:      "claude-code",
			ReplicaID:  testOpaque('e'),
			DeviceID:   testOpaque('d'),
			Generation: 1,
		},
		Parents:         []string{testOpaque('h')},
		Ranges:          []RangeRef{testRange(testOpaque('e'), 0, 4)},
		EnvironmentRefs: []string{testOpaque('i')},
		CreatedAt:       testTime(),
	}
}

func testMaterializedBinding() LocalBinding {
	return LocalBinding{
		Version:            ModelVersion,
		HubID:              testOpaque('a'),
		ProjectID:          testOpaque('b'),
		SessionID:          testOpaque('c'),
		Agent:              "codex",
		NativeSessionID:    "local-native-session-id",
		ReplicaID:          testOpaque('j'),
		Generation:         1,
		ReplicaCursor:      ReplicaCursor{NextShard: 2, RecordCount: 4, HeadDigest: testDigest('0')},
		ContributionCursor: ContributionCursor{EndRecord: 4, LastContributionID: testOpaque('g')},
		Origin: BindingOrigin{
			Kind:      ReplicaOriginLocalMaterialize,
			BaseHeads: []string{testOpaque('h')},
			ImportBoundary: &ImportBoundary{
				RecordCount:  3,
				PrefixDigest: testDigest('2'),
			},
			Converter: &ConverterProvenance{
				SourceViewVersion:    1,
				TargetAdapterVersion: "codex-v1",
			},
		},
	}
}

func TestValidateHierarchyAndParentRelationships(t *testing.T) {
	hub, project, session := testHub(), testProject(), testSession()
	if err := ValidateHierarchy(hub, project, session); err != nil {
		t.Fatalf("valid hierarchy rejected: %v", err)
	}

	project.HubID = testOpaque('z')
	if err := ValidateHierarchy(hub, project, session); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("hub mismatch error = %v, want ErrInvalidHierarchy", err)
	}

	project = testProject()
	session.ProjectID = testOpaque('z')
	if err := ValidateHierarchy(hub, project, session); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("project mismatch error = %v, want ErrInvalidHierarchy", err)
	}

	if err := ValidateReplicaForSession(testReplica(), testSession()); err != nil {
		t.Fatalf("valid replica relationship rejected: %v", err)
	}
	wrongReplica := testReplica()
	wrongReplica.SessionID = testOpaque('z')
	if err := ValidateReplicaForSession(wrongReplica, testSession()); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("wrong replica relationship error = %v, want ErrInvalidHierarchy", err)
	}

	if err := ValidateContributionForSession(testContribution(), testSession()); err != nil {
		t.Fatalf("valid contribution relationship rejected: %v", err)
	}
}

func TestDescriptorAndLocalEnvelopeRoundTrips(t *testing.T) {
	tests := []struct {
		name  string
		data  func() ([]byte, error)
		parse func([]byte) (any, error)
		want  any
	}{
		{
			name: "hub",
			data: func() ([]byte, error) { return testHub().MarshalBinary() },
			parse: func(data []byte) (any, error) {
				value, err := ParseHubDescriptor(data)
				return value, err
			},
			want: testHub(),
		},
		{
			name: "project",
			data: func() ([]byte, error) { return testProject().MarshalBinary() },
			parse: func(data []byte) (any, error) {
				value, err := ParseProjectDescriptor(data)
				return value, err
			},
			want: testProject(),
		},
		{
			name: "session",
			data: func() ([]byte, error) { return testSession().MarshalBinary() },
			parse: func(data []byte) (any, error) {
				value, err := ParseSessionDescriptor(data)
				return value, err
			},
			want: testSession(),
		},
		{
			name: "replica",
			data: func() ([]byte, error) { return testReplica().MarshalBinary() },
			parse: func(data []byte) (any, error) {
				value, err := ParseNativeReplicaDescriptor(data)
				return value, err
			},
			want: testReplica(),
		},
		{
			name: "tip",
			data: func() ([]byte, error) { return testTip().MarshalBinary() },
			parse: func(data []byte) (any, error) {
				value, err := ParseReplicaTip(data)
				return value, err
			},
			want: testTip(),
		},
		{
			name: "contribution",
			data: func() ([]byte, error) { return testContribution().MarshalBinary() },
			parse: func(data []byte) (any, error) {
				value, err := ParseContribution(data)
				return value, err
			},
			want: testContribution(),
		},
		{
			name: "binding",
			data: func() ([]byte, error) { return testMaterializedBinding().MarshalBinary() },
			parse: func(data []byte) (any, error) {
				value, err := ParseLocalBinding(data)
				return value, err
			},
			want: testMaterializedBinding(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := tt.data()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := tt.parse(data)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("round trip = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEnvelopeParsingRejectsNonCanonicalOrUnknownInput(t *testing.T) {
	data, err := testHub().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	unknownField := append([]byte(nil), bytes.TrimSuffix(data, []byte("}"))...)
	unknownField = append(unknownField, []byte(`,"unknown":1}`)...)
	for name, input := range map[string][]byte{
		"leading whitespace":  append([]byte(" "), data...),
		"trailing whitespace": append(append([]byte(nil), data...), '\n'),
		"trailing json":       append(append([]byte(nil), data...), []byte("{}")...),
		"unknown field":       unknownField,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseHubDescriptor(input); !errors.Is(err, ErrInvalidEnvelope) {
				t.Fatalf("error = %v, want ErrInvalidEnvelope", err)
			}
		})
	}

	newer := bytes.Replace(data, []byte(`"version":1`), []byte(`"version":2`), 1)
	if _, err := ParseHubDescriptor(newer); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("newer version error = %v, want ErrUnsupportedVersion", err)
	}
}

func TestContributionCanonicalizationAndDerivedID(t *testing.T) {
	first := testContribution()
	first.ContributionID = ""
	first.Parents = []string{testOpaque('h'), testOpaque('g')}
	first.EnvironmentRefs = []string{testOpaque('i'), testOpaque('f')}
	first.Ranges = []RangeRef{
		testRange(testOpaque('e'), 4, 8),
		testRange(testOpaque('e'), 0, 4),
	}
	original := first

	key := strings.Repeat("k", 32)
	derived, err := first.WithDerivedID([]byte(key))
	if err != nil {
		t.Fatalf("derive contribution ID: %v", err)
	}
	if !reflect.DeepEqual(first, original) {
		t.Fatal("WithDerivedID mutated the input contribution")
	}
	if !sortStrings(derived.Parents) || !sortStrings(derived.EnvironmentRefs) {
		t.Fatalf("lists were not canonicalized: %#v", derived)
	}
	if derived.Ranges[0].StartRecord != 0 || derived.Ranges[1].StartRecord != 4 {
		t.Fatalf("ranges were not canonicalized: %#v", derived.Ranges)
	}

	second := first
	second.CreatedAt = testTime().Add(24 * time.Hour)
	second.Parents = []string{testOpaque('g'), testOpaque('h')}
	second.EnvironmentRefs = []string{testOpaque('f'), testOpaque('i')}
	second.Ranges = []RangeRef{first.Ranges[1], first.Ranges[0]}
	derivedAgain, err := second.WithDerivedID([]byte(key))
	if err != nil {
		t.Fatalf("derive second contribution ID: %v", err)
	}
	if derived.ContributionID != derivedAgain.ContributionID {
		t.Fatal("reordered or retimestamped contribution changed its derived ID")
	}

	encoded, err := derived.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseContribution(encoded)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := parsed.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, canonical) {
		t.Fatal("canonical contribution changed after parse")
	}
}

func TestContributionValidationRejectsUnsafeRelationships(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Contribution)
	}{
		{
			name: "empty range",
			edit: func(c *Contribution) { c.Ranges[0].EndRecord = c.Ranges[0].StartRecord },
		},
		{
			name: "overlapping ranges",
			edit: func(c *Contribution) { c.Ranges = append(c.Ranges, testRange(testOpaque('e'), 3, 7)) },
		},
		{
			name: "foreign replica",
			edit: func(c *Contribution) { c.Ranges[0].ReplicaID = testOpaque('z') },
		},
		{
			name: "duplicate parent",
			edit: func(c *Contribution) { c.Parents = []string{testOpaque('h'), testOpaque('h')} },
		},
		{
			name: "missing id",
			edit: func(c *Contribution) { c.ContributionID = "" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := testContribution()
			tt.edit(&value)
			if err := value.Validate(); !errors.Is(err, ErrInvalidModel) && !errors.Is(err, ErrInvalidIdentity) {
				t.Fatalf("error = %v, want model/identity validation error", err)
			}
		})
	}
}

func TestOriginAndBindingValidationRejectConversionInconsistency(t *testing.T) {
	origin := BindingOrigin{Kind: ReplicaOriginNative, BaseHeads: []string{testOpaque('h')}}
	if err := origin.Validate(); !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("native origin with heads error = %v, want ErrInvalidModel", err)
	}

	binding := testMaterializedBinding()
	binding.ContributionCursor.EndRecord = 2
	if err := binding.Validate(); !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("cursor before boundary error = %v, want ErrInvalidModel", err)
	}

	binding = testMaterializedBinding()
	binding.ContributionCursor.EndRecord = 5
	if err := binding.Validate(); !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("contribution cursor ahead error = %v, want ErrInvalidModel", err)
	}
}

func TestReplicaDescriptorDoesNotSerializePlainNativeSessionID(t *testing.T) {
	data, err := testReplica().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("local-native-session-id")) {
		t.Fatal("Replica descriptor serialized a plaintext native session ID")
	}
}

func sortStrings(values []string) bool {
	return len(values) == 0 || strings.Join(values, "\x00") == strings.Join(sortedStrings(values), "\x00")
}
