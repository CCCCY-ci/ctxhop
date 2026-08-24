package syncer

import (
	"encoding/hex"
	"testing"
)

func FuzzParseShard(f *testing.F) {
	digest := EmptyDigest()
	seed := []byte(`{"version":1,"base":0,"count":1,"prefixDigest":"` + hex.EncodeToString(digest[:]) + `","records":[{"ok":true}]}`)
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseShard(data)
	})
}
