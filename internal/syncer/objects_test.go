package syncer

import (
	"bytes"
	"errors"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestObjectLayoutBuildsSafeDeviceOwnedKeys(t *testing.T) {
	layout, err := NewObjectLayout("projectid", "sessionid", "deviceid")
	if err != nil {
		t.Fatal(err)
	}

	prefix, err := layout.SessionPrefix()
	if err != nil {
		t.Fatal(err)
	}
	if prefix != "v1/projects/projectid/sessions/sessionid" {
		t.Fatalf("session prefix = %q", prefix)
	}
	devicePrefix, err := layout.DevicePrefix()
	if err != nil {
		t.Fatal(err)
	}
	if devicePrefix != prefix+"/deviceid" {
		t.Fatalf("device prefix = %q", devicePrefix)
	}
	meta, err := layout.MetadataKey()
	if err != nil {
		t.Fatal(err)
	}
	if meta != devicePrefix+"/meta" {
		t.Fatalf("metadata key = %q", meta)
	}
	shard, err := layout.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	if shard != devicePrefix+"/000001" {
		t.Fatalf("shard key = %q", shard)
	}
	if err := remote.ValidateKey(shard); err != nil {
		t.Fatalf("generated key is not valid: %v", err)
	}

	parsed, err := ParseShardNumber("000042")
	if err != nil || parsed != 42 {
		t.Fatalf("ParseShardNumber = %d, %v", parsed, err)
	}
}

func TestObjectLayoutRejectsUnsafeIdentifiersAndSequences(t *testing.T) {
	for _, values := range [][3]string{
		{"", "session", "device"},
		{"Project", "session", "device"},
		{"project", "session/id", "device"},
		{"project", "session", "device\\id"},
		{"project", "..", "device"},
	} {
		if _, err := NewObjectLayout(values[0], values[1], values[2]); err == nil {
			t.Errorf("NewObjectLayout(%q) unexpectedly succeeded", values)
		}
	}
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	for _, number := range []uint64{0, maxShardNumber + 1} {
		if _, err := layout.ShardKey(number); err == nil {
			t.Errorf("ShardKey(%d) unexpectedly succeeded", number)
		}
	}
	for _, name := range []string{"", "000000", "1000000", "00000a", "000001/"} {
		if _, err := ParseShardNumber(name); err == nil {
			t.Errorf("ParseShardNumber(%q) unexpectedly succeeded", name)
		}
	}
	var zero ObjectLayout
	if _, err := zero.DevicePrefix(); err == nil {
		t.Fatal("zero ObjectLayout unexpectedly produced a prefix")
	}
}

func TestSealAndOpenShardBindsCiphertextToTheObjectKey(t *testing.T) {
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	key, err := layout.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	shard, err := NewShard(0, EmptyDigest(), [][]byte{[]byte(`{"message":"synthetic"}`)})
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
	sealed, err := SealShard(public, key, shard)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("synthetic")) {
		t.Fatal("ciphertext contains shard plaintext")
	}
	got, err := OpenShard(private, key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 1 || !bytes.Equal(got.Records[0], shard.Records[0]) {
		t.Fatalf("opened shard = %+v", got)
	}

	if _, err := OpenShard(private, key+"/wrong", sealed); err == nil {
		t.Fatal("ciphertext opened under a different object key")
	}
	other := crypto.NewDataKey()
	defer other.Close()
	otherPrivate, err := other.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenShard(otherPrivate, key, sealed); err == nil {
		t.Fatal("ciphertext opened with a different identity key")
	}
}

func TestSealAndOpenShardRejectInvalidObjectKeysAndCiphertext(t *testing.T) {
	shard, err := NewShard(0, EmptyDigest(), [][]byte{[]byte(`{"ok":true}`)})
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
	for _, key := range []string{"", "/absolute", "v1/../escape", "v1\\escape"} {
		if _, err := SealShard(public, key, shard); err == nil || !errors.Is(err, remote.ErrInvalidKey) {
			t.Errorf("SealShard(%q) error = %v, want remote.ErrInvalidKey", key, err)
		}
		if _, err := OpenShard(private, key, []byte("not ciphertext")); err == nil || !errors.Is(err, remote.ErrInvalidKey) {
			t.Errorf("OpenShard(%q) error = %v, want remote.ErrInvalidKey", key, err)
		}
	}
	if _, err := OpenShard(private, "v1/projects/p/s/d/000001", []byte("not ciphertext")); err == nil {
		t.Fatal("corrupt ciphertext unexpectedly opened")
	}
}
