package syncer

import (
	"crypto/ecdh"
	"errors"
)

func validateIdentities(identities []*ecdh.PrivateKey) error {
	if len(identities) == 0 {
		return errors.New("syncer: at least one identity key is required")
	}
	for _, identity := range identities {
		if identity == nil {
			return errors.New("syncer: identity key is required")
		}
	}
	return nil
}

func openMetadataWithIdentities(identities []*ecdh.PrivateKey, objectKey string, sealed []byte) (Metadata, error) {
	var last error
	for _, identity := range identities {
		metadata, err := OpenMetadata(identity, objectKey, sealed)
		if err == nil {
			return metadata, nil
		}
		last = err
	}
	return Metadata{}, last
}

func openDeviceRecordWithIdentities(identities []*ecdh.PrivateKey, objectKey string, sealed []byte) (DeviceRecord, error) {
	var last error
	for _, identity := range identities {
		record, err := OpenDeviceRecord(identity, objectKey, sealed)
		if err == nil {
			return record, nil
		}
		last = err
	}
	return DeviceRecord{}, last
}

func openShardWithIdentities(identities []*ecdh.PrivateKey, objectKey string, sealed []byte) (Shard, error) {
	var last error
	for _, identity := range identities {
		shard, err := OpenShard(identity, objectKey, sealed)
		if err == nil {
			return shard, nil
		}
		last = err
	}
	return Shard{}, last
}
