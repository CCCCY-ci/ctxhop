package syncer

import (
	"errors"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
)

func TestObjectLayoutMethodsRejectInvalidStoredState(t *testing.T) {
	var zero ObjectLayout
	if _, err := zero.MetadataKey(); err == nil {
		t.Fatal("zero MetadataKey unexpectedly succeeded")
	}
	if _, err := zero.ShardKey(1); err == nil {
		t.Fatal("zero ShardKey unexpectedly succeeded")
	}
	if _, err := (ObjectLayout{projectID: "p"}).SessionPrefix(); err == nil {
		t.Fatal("incomplete session layout unexpectedly succeeded")
	}
	if _, err := (ObjectLayout{projectID: "p", sessionID: "s"}).DevicePrefix(); err == nil {
		t.Fatal("incomplete device layout unexpectedly succeeded")
	}
	if _, err := checkedKey("v1/../unsafe"); err == nil {
		t.Fatal("checkedKey accepted an unsafe key")
	}
}

func TestObjectEncryptionRejectsMissingKeysAndInvalidPlaintext(t *testing.T) {
	layout, err := NewObjectLayout("p", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	key, err := layout.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := SealShard(nil, key, shard); err == nil {
		t.Fatal("SealShard accepted a nil recipient")
	}
	if _, err := OpenShard(nil, key, []byte("ciphertext")); err == nil {
		t.Fatal("OpenShard accepted a nil identity")
	}

	sealed, err := crypto.Encrypt(public, key, []byte("not a shard"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenShard(private, key, sealed); err == nil || !errors.Is(err, ErrInvalidShard) {
		t.Fatalf("OpenShard invalid plaintext error = %v, want ErrInvalidShard", err)
	}
}
