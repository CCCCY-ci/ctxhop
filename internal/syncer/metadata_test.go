package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

func TestMetadataRoundTripIsDeterministicAndKeyBound(t *testing.T) {
	payload := []byte(`{"session":"opaque","version":1}`)
	metadata, err := NewMetadata(0, EmptyDigest(), payload)
	if err != nil {
		t.Fatal(err)
	}
	payload[2] = 'X'
	if bytes.Equal(metadata.Payload, payload) {
		t.Fatal("metadata retained the caller-owned payload")
	}

	first, err := metadata.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	second, err := metadata.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("encoding the same metadata twice produced different bytes")
	}
	if strings.Contains(string(first), " ") || strings.Contains(string(first), "\\u003c") {
		t.Fatalf("metadata was not compact and unescaped: %s", first)
	}
	parsed, err := ParseMetadata(first)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RecordCount != metadata.RecordCount || parsed.HeadDigest != metadata.HeadDigest || !bytes.Equal(parsed.Payload, metadata.Payload) {
		t.Fatalf("parsed metadata = %+v, want %+v", parsed, metadata)
	}

	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	key, err := layout.MetadataKey()
	if err != nil {
		t.Fatal(err)
	}
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
	sealed, err := SealMetadata(public, key, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, metadata.Payload) {
		t.Fatal("ciphertext contains metadata plaintext")
	}
	opened, err := OpenMetadata(private, key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened.Payload, metadata.Payload) {
		t.Fatalf("opened payload = %s, want %s", opened.Payload, metadata.Payload)
	}
	if _, err := OpenMetadata(private, key+"/wrong", sealed); err == nil {
		t.Fatal("metadata opened under a different object key")
	}
	other := crypto.NewDataKey()
	defer other.Close()
	otherPrivate, err := other.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenMetadata(otherPrivate, key, sealed); err == nil {
		t.Fatal("metadata opened with a different identity key")
	}
}

func TestMetadataRejectsMalformedEnvelopesAndPayloads(t *testing.T) {
	digest := EmptyDigest()
	valid := fmt.Sprintf(`{"version":1,"recordCount":0,"headDigest":"%s","payload":{"ok":true}}`, hex.EncodeToString(digest[:]))
	for name, input := range map[string]string{
		"empty":          "",
		"trailing json":  valid + `{}`,
		"trailing text":  valid + `x`,
		"unknown field":  strings.TrimSuffix(valid, `}`) + `,"extra":1}`,
		"wrong version":  strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"bad digest":     strings.Replace(valid, hex.EncodeToString(digest[:]), strings.Repeat("0", 63), 1),
		"pretty payload": strings.Replace(valid, `{"ok":true}`, `{ "ok": true }`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseMetadata([]byte(input)); err == nil || !errors.Is(err, ErrInvalidMetadata) {
				t.Fatalf("ParseMetadata(%q) error = %v, want ErrInvalidMetadata", name, err)
			}
		})
	}

	for name, payload := range map[string][]byte{
		"empty":        nil,
		"invalid json": []byte("not json"),
		"pretty json":  []byte(`{ "ok": true }`),
	} {
		t.Run("new "+name, func(t *testing.T) {
			if _, err := NewMetadata(0, EmptyDigest(), payload); err == nil || !errors.Is(err, ErrInvalidMetadata) {
				t.Fatalf("NewMetadata(%q) error = %v, want ErrInvalidMetadata", name, err)
			}
		})
	}
	if _, err := NewMetadata(0, [32]byte{1}, []byte(`{"ok":true}`)); err == nil || !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("empty-prefix digest error = %v, want ErrInvalidMetadata", err)
	}
	if _, err := NewMetadata(1, EmptyDigest(), bytes.Repeat([]byte{'x'}, maxMetadataBytes+1)); err == nil || !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("oversized payload error = %v, want ErrInvalidMetadata", err)
	}
}

func TestPutAndFetchMetadataSortsDevicesAndIgnoresShards(t *testing.T) {
	root := t.TempDir()
	store, err := remote.NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
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
	layoutB, err := NewObjectLayout("project", "session", "deviceb")
	if err != nil {
		t.Fatal(err)
	}
	layoutA, err := NewObjectLayout("project", "session", "devicea")
	if err != nil {
		t.Fatal(err)
	}
	metadataA, err := NewMetadata(1, [32]byte{1}, []byte(`{"device":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	metadataB, err := NewMetadata(2, [32]byte{2}, []byte(`{"device":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := PutMetadata(context.Background(), store, public, layoutB, metadataB); err != nil {
		t.Fatal(err)
	}
	if err := PutMetadata(context.Background(), store, public, layoutA, metadataA); err != nil {
		t.Fatal(err)
	}

	shard, err := NewShard(0, EmptyDigest(), [][]byte{[]byte(`{"n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	shardKey, err := layoutA.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	sealedShard, err := SealShard(public, shardKey, shard)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), shardKey, bytes.NewReader(sealedShard), int64(len(sealedShard))); err != nil {
		t.Fatal(err)
	}

	refs, err := FetchMetadata(context.Background(), store, "project", "session", private)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].DeviceID != "devicea" || refs[1].DeviceID != "deviceb" {
		t.Fatalf("metadata refs = %+v", refs)
	}
	if !bytes.Equal(refs[0].Metadata.Payload, metadataA.Payload) || !bytes.Equal(refs[1].Metadata.Payload, metadataB.Payload) {
		t.Fatalf("metadata refs payloads = %q, %q", refs[0].Metadata.Payload, refs[1].Metadata.Payload)
	}
}

func TestFetchMetadataRejectsRemoteFailuresDuplicatesAndOversize(t *testing.T) {
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
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := NewMetadata(0, EmptyDigest(), []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("empty", func(t *testing.T) {
		store := &remoteReadFake{objects: map[string][]byte{}}
		if _, err := FetchMetadata(context.Background(), store, "project", "session", private); !errors.Is(err, ErrNoRemoteMetadata) {
			t.Fatalf("FetchMetadata error = %v, want ErrNoRemoteMetadata", err)
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		store := &remoteReadFake{objects: map[string][]byte{}}
		addRemoteMetadata(t, store, layout, public, metadata)
		key, err := layout.MetadataKey()
		if err != nil {
			t.Fatal(err)
		}
		store.list = append(store.list, remote.ObjectInfo{Key: key})
		if _, err := FetchMetadata(context.Background(), store, "project", "session", private); !errors.Is(err, ErrDuplicateMetadata) {
			t.Fatalf("FetchMetadata error = %v, want ErrDuplicateMetadata", err)
		}
	})
	t.Run("oversize", func(t *testing.T) {
		key, err := layout.MetadataKey()
		if err != nil {
			t.Fatal(err)
		}
		store := &remoteReadFake{
			objects: map[string][]byte{key: bytes.Repeat([]byte{'x'}, maxEncryptedMetadataBytes+1)},
			list:    []remote.ObjectInfo{{Key: key}},
		}
		if _, err := FetchMetadata(context.Background(), store, "project", "session", private); !errors.Is(err, ErrRemoteMetadataTooLarge) {
			t.Fatalf("FetchMetadata error = %v, want ErrRemoteMetadataTooLarge", err)
		}
	})
	t.Run("list failure", func(t *testing.T) {
		store := &remoteReadFake{listErr: errors.New("list failed")}
		if _, err := FetchMetadata(context.Background(), store, "project", "session", private); err == nil {
			t.Fatal("FetchMetadata unexpectedly succeeded")
		}
	})
	t.Run("get failure", func(t *testing.T) {
		key, err := layout.MetadataKey()
		if err != nil {
			t.Fatal(err)
		}
		store := &remoteReadFake{getErr: errors.New("get failed"), list: []remote.ObjectInfo{{Key: key}}}
		if _, err := FetchMetadata(context.Background(), store, "project", "session", private); err == nil {
			t.Fatal("FetchMetadata unexpectedly succeeded")
		}
	})
}

func TestMetadataHandlesCancellationAndArguments(t *testing.T) {
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
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := NewMetadata(0, EmptyDigest(), []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	store := &remoteReadFake{objects: map[string][]byte{}}
	if err := PutMetadata(context.Background(), store, public, layout, metadata); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := PutMetadata(cancelled, store, public, layout, metadata); err == nil {
		t.Fatal("cancelled PutMetadata unexpectedly succeeded")
	}
	if _, err := FetchMetadata(cancelled, store, "project", "session", private); err == nil {
		t.Fatal("cancelled FetchMetadata unexpectedly succeeded")
	}
	if err := PutMetadata(nil, store, public, layout, metadata); err == nil {
		t.Fatal("nil context PutMetadata unexpectedly succeeded")
	}
	if err := PutMetadata(context.Background(), nil, public, layout, metadata); err == nil {
		t.Fatal("nil store PutMetadata unexpectedly succeeded")
	}
	if err := PutMetadata(context.Background(), store, nil, layout, metadata); err == nil {
		t.Fatal("nil recipient PutMetadata unexpectedly succeeded")
	}
	if _, err := FetchMetadata(nil, store, "project", "session", private); err == nil {
		t.Fatal("nil context FetchMetadata unexpectedly succeeded")
	}
	if _, err := FetchMetadata(context.Background(), nil, "project", "session", private); err == nil {
		t.Fatal("nil store FetchMetadata unexpectedly succeeded")
	}
	if _, err := FetchMetadata(context.Background(), store, "project", "session", nil); err == nil {
		t.Fatal("nil identity FetchMetadata unexpectedly succeeded")
	}
	if _, err := SealMetadata(public, "", metadata); err == nil {
		t.Fatal("empty metadata key unexpectedly succeeded")
	}
	if _, err := OpenMetadata(private, "", []byte("ciphertext")); err == nil {
		t.Fatal("empty metadata key unexpectedly succeeded")
	}
}

func addRemoteMetadata(t *testing.T, store *remoteReadFake, layout ObjectLayout, public *ecdh.PublicKey, metadata Metadata) {
	t.Helper()
	key, err := layout.MetadataKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealMetadata(public, key, metadata)
	if err != nil {
		t.Fatal(err)
	}
	store.objects[key] = sealed
	store.list = append(store.list, remote.ObjectInfo{Key: key, Size: int64(len(sealed))})
}
