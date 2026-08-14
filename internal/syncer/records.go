// Package syncer coordinates canonical Agent records, encrypted remote objects,
// and safe local restoration.
//
// The first part of that job is deliberately independent of I/O: this file
// defines the immutable record shard and its digest chain. Remote objects are
// untrusted bytes until this layer validates them.
package syncer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	shardVersion  = 1
	maxShardBytes = 64 << 20
	digestDomain  = "agentsync/records/v1\x00"
)

var (
	// ErrInvalidRecord reports a value that cannot be part of a canonical
	// record stream.
	ErrInvalidRecord = errors.New("syncer: invalid canonical record")

	// ErrInvalidShard reports an invalid or unsupported shard envelope.
	ErrInvalidShard = errors.New("syncer: invalid shard")

	// ErrIncompleteBranch reports a device stream with a missing or unusable
	// shard. The available prefix must not be treated as a complete session.
	ErrIncompleteBranch = errors.New("syncer: incomplete device branch")
)

// Shard is an immutable range of canonical session records.
//
// Base is the number of records before this shard. PrefixDigest is the digest
// after those records. Records are copied when a Shard is constructed or
// decoded, so callers can safely retain their input buffers.
type Shard struct {
	Base         uint64
	PrefixDigest [32]byte
	Records      [][]byte
}

// NewShard validates and constructs a shard for records beginning at base.
func NewShard(base uint64, prefixDigest [32]byte, records [][]byte) (Shard, error) {
	if len(records) == 0 {
		return Shard{}, fmt.Errorf("%w: a shard must contain at least one record", ErrInvalidShard)
	}

	copyRecords, err := copyRecords(records)
	if err != nil {
		return Shard{}, fmt.Errorf("%w: %v", ErrInvalidShard, err)
	}

	shard := Shard{
		Base:         base,
		PrefixDigest: prefixDigest,
		Records:      copyRecords,
	}
	if err := shard.Validate(); err != nil {
		return Shard{}, err
	}
	return shard, nil
}

// Count returns the number of records in the shard.
func (s Shard) Count() uint64 {
	return uint64(len(s.Records))
}

// Digest returns the digest immediately after this shard.
func (s Shard) Digest() [32]byte {
	digest := s.PrefixDigest
	for _, record := range s.Records {
		digest = nextDigest(digest, record)
	}
	return digest
}

// Validate checks the record shape and the shard's own digest chain.
func (s Shard) Validate() error {
	if len(s.Records) == 0 {
		return fmt.Errorf("%w: a shard must contain at least one record", ErrInvalidShard)
	}
	for i, record := range s.Records {
		if err := validateRecord(record); err != nil {
			return fmt.Errorf("%w: record %d: %v", ErrInvalidShard, i+1, err)
		}
	}
	return nil
}

// MarshalBinary encodes a shard as its deterministic plaintext envelope.
// Callers must encrypt the returned bytes before sending them to a Remote.
func (s Shard) MarshalBinary() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	wire := shardWire{
		Version:      shardVersion,
		Base:         s.Base,
		Count:        s.Count(),
		PrefixDigest: hex.EncodeToString(s.PrefixDigest[:]),
		Records:      make([]json.RawMessage, len(s.Records)),
	}
	for i, record := range s.Records {
		wire.Records[i] = append(json.RawMessage(nil), record...)
	}

	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(wire); err != nil {
		return nil, fmt.Errorf("encode shard: %w", err)
	}
	data := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: encoded shard is empty", ErrInvalidShard)
	}
	if len(data) > maxShardBytes {
		return nil, fmt.Errorf("%w: encoded shard exceeds %d bytes", ErrInvalidShard, maxShardBytes)
	}
	return data, nil
}

// ParseShard validates and decodes a shard received from an untrusted source.
func ParseShard(data []byte) (Shard, error) {
	if len(data) == 0 {
		return Shard{}, fmt.Errorf("%w: empty envelope", ErrInvalidShard)
	}
	if len(data) > maxShardBytes {
		return Shard{}, fmt.Errorf("%w: encoded shard exceeds %d bytes", ErrInvalidShard, maxShardBytes)
	}

	var wire shardWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Shard{}, fmt.Errorf("%w: decode envelope: %v", ErrInvalidShard, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Shard{}, fmt.Errorf("%w: envelope contains trailing JSON", ErrInvalidShard)
	} else if !errors.Is(err, io.EOF) {
		// A second JSON value is handled above. Any non-EOF error means the
		// input was not a complete single JSON value.
		return Shard{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidShard, err)
	}

	if wire.Version != shardVersion {
		return Shard{}, fmt.Errorf("%w: unsupported shard version %d", ErrInvalidShard, wire.Version)
	}
	if wire.Count == 0 || wire.Count != uint64(len(wire.Records)) {
		return Shard{}, fmt.Errorf("%w: count does not match records", ErrInvalidShard)
	}
	prefix, err := parseDigest(wire.PrefixDigest)
	if err != nil {
		return Shard{}, fmt.Errorf("%w: prefix digest: %v", ErrInvalidShard, err)
	}

	records := make([][]byte, len(wire.Records))
	for i, record := range wire.Records {
		records[i] = append([]byte(nil), record...)
	}
	return NewShard(wire.Base, prefix, records)
}

// EmptyDigest returns the digest for an empty record prefix.
func EmptyDigest() [32]byte {
	return sha256.Sum256([]byte(digestDomain))
}

// DigestRecords returns the digest after all records in order.
func DigestRecords(records [][]byte) ([32]byte, error) {
	digest := EmptyDigest()
	for i, record := range records {
		if err := validateRecord(record); err != nil {
			return [32]byte{}, fmt.Errorf("record %d: %w", i+1, err)
		}
		digest = nextDigest(digest, record)
	}
	return digest, nil
}

type shardWire struct {
	Version      int               `json:"version"`
	Base         uint64            `json:"base"`
	Count        uint64            `json:"count"`
	PrefixDigest string            `json:"prefixDigest"`
	Records      []json.RawMessage `json:"records"`
}

func copyRecords(records [][]byte) ([][]byte, error) {
	copyRecords := make([][]byte, len(records))
	for i, record := range records {
		if err := validateRecord(record); err != nil {
			return nil, fmt.Errorf("record %d: %w", i+1, err)
		}
		copyRecords[i] = append([]byte(nil), record...)
	}
	return copyRecords, nil
}

func validateRecord(record []byte) error {
	if len(record) == 0 {
		return fmt.Errorf("%w: empty record", ErrInvalidRecord)
	}
	if bytes.ContainsAny(record, "\r\n") {
		return fmt.Errorf("%w: record must be one line", ErrInvalidRecord)
	}
	if !json.Valid(record) {
		return fmt.Errorf("%w: record is not JSON", ErrInvalidRecord)
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, record); err != nil {
		return fmt.Errorf("%w: compact record: %v", ErrInvalidRecord, err)
	}
	if !bytes.Equal(compact.Bytes(), record) {
		return fmt.Errorf("%w: record is not compact", ErrInvalidRecord)
	}
	return nil
}

func parseDigest(value string) ([32]byte, error) {
	var digest [32]byte
	if len(value) != hex.EncodedLen(len(digest)) || value != strings.ToLower(value) {
		return digest, errors.New("must be 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return digest, errors.New("must be 64 lowercase hexadecimal characters")
	}
	copy(digest[:], decoded)
	return digest, nil
}

func nextDigest(previous [32]byte, record []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte(digestDomain))
	_, _ = h.Write(previous[:])
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(record)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(record)

	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
