package syncer

import (
	"bytes"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
)

func TestSealShardCompressesBeforeEncryptionAndReadsLegacyObjects(t *testing.T) {
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	key, err := layout.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}

	records := make([][]byte, 512)
	for i := range records {
		records[i] = []byte(`{"message":"repeated session content for compression"}`)
	}
	shard, err := NewShard(0, EmptyDigest(), records)
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
	decrypted, err := crypto.Decrypt(private, key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(decrypted, []byte(compressionMagic)) {
		t.Fatal("new shard object is not compressed before encryption")
	}

	got, err := OpenShard(private, key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != len(shard.Records) || !bytes.Equal(got.Records[0], shard.Records[0]) {
		t.Fatalf("opened compressed shard = %+v", got)
	}

	legacyPlaintext, err := shard.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	legacySealed, err := crypto.Encrypt(public, key, legacyPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	legacyGot, err := OpenShard(private, key, legacySealed)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyGot.Records) != len(shard.Records) || !bytes.Equal(legacyGot.Records[0], shard.Records[0]) {
		t.Fatalf("opened legacy shard = %+v", legacyGot)
	}
}
