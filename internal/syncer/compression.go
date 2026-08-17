package syncer

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	compressionMagic      = "ASc1"
	compressionVersion    = byte(1)
	compressionCodecZlib  = byte(1)
	compressionHeaderSize = len(compressionMagic) + 1 + 1 + 8 + 8
)

var (
	// ErrInvalidCompressedPayload reports a malformed compressed payload.
	ErrInvalidCompressedPayload = errors.New("syncer: invalid compressed payload")

	// ErrUnsupportedCompression reports a compression version or codec that
	// this build does not understand.
	ErrUnsupportedCompression = errors.New("syncer: unsupported compression format")

	// ErrCompressedPayloadTooLarge reports a payload that exceeds the
	// decompression or shard size limit.
	ErrCompressedPayloadTooLarge = errors.New("syncer: compressed payload exceeds size limit")
)

// compressPayload compresses a serialized shard when doing so reduces its
// size. If compression would expand the payload, the original bytes are
// returned so small or incompressible shards never grow.
func compressPayload(payload []byte, maxSize int) ([]byte, error) {
	if err := validateCompressionLimit(maxSize); err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: payload is empty", ErrInvalidCompressedPayload)
	}
	if len(payload) > maxSize {
		return nil, fmt.Errorf("%w: payload is %d bytes, limit is %d", ErrCompressedPayloadTooLarge, len(payload), maxSize)
	}

	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("syncer: compress payload: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("syncer: finalize compressed payload: %w", err)
	}

	if compressed.Len() >= len(payload)-compressionHeaderSize {
		return append([]byte(nil), payload...), nil
	}

	wire := make([]byte, compressionHeaderSize+compressed.Len())
	copy(wire, compressionMagic)
	wire[len(compressionMagic)] = compressionVersion
	wire[len(compressionMagic)+1] = compressionCodecZlib
	binary.BigEndian.PutUint64(wire[len(compressionMagic)+2:len(compressionMagic)+10], uint64(len(payload)))
	binary.BigEndian.PutUint64(wire[len(compressionMagic)+10:compressionHeaderSize], uint64(compressed.Len()))
	copy(wire[compressionHeaderSize:], compressed.Bytes())
	return wire, nil
}

// decompressPayload expands a current compressed payload or returns a copy of
// a legacy uncompressed payload. The legacy path keeps old remote objects
// readable after compression is enabled.
func decompressPayload(payload []byte, maxSize int) ([]byte, error) {
	if err := validateCompressionLimit(maxSize); err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: payload is empty", ErrInvalidCompressedPayload)
	}
	if !bytes.HasPrefix(payload, []byte(compressionMagic)) {
		if len(payload) > maxSize {
			return nil, fmt.Errorf("%w: legacy payload is %d bytes, limit is %d", ErrCompressedPayloadTooLarge, len(payload), maxSize)
		}
		return append([]byte(nil), payload...), nil
	}
	if len(payload) < compressionHeaderSize {
		return nil, fmt.Errorf("%w: compressed header is truncated", ErrInvalidCompressedPayload)
	}

	version := payload[len(compressionMagic)]
	if version != compressionVersion {
		return nil, fmt.Errorf("%w: version %d", ErrUnsupportedCompression, version)
	}
	if codec := payload[len(compressionMagic)+1]; codec != compressionCodecZlib {
		return nil, fmt.Errorf("%w: codec %d", ErrUnsupportedCompression, codec)
	}

	uncompressedLen := binary.BigEndian.Uint64(payload[len(compressionMagic)+2 : len(compressionMagic)+10])
	compressedLen := binary.BigEndian.Uint64(payload[len(compressionMagic)+10 : compressionHeaderSize])
	if uncompressedLen == 0 || compressedLen == 0 {
		return nil, fmt.Errorf("%w: payload lengths must be non-zero", ErrInvalidCompressedPayload)
	}
	if uncompressedLen > uint64(maxSize) {
		return nil, fmt.Errorf("%w: decompressed size %d exceeds limit %d", ErrCompressedPayloadTooLarge, uncompressedLen, maxSize)
	}
	if len(payload)-compressionHeaderSize > maxSize {
		return nil, fmt.Errorf("%w: compressed size exceeds limit %d", ErrCompressedPayloadTooLarge, maxSize)
	}
	if compressedLen != uint64(len(payload)-compressionHeaderSize) {
		return nil, fmt.Errorf("%w: compressed length does not match payload", ErrInvalidCompressedPayload)
	}

	compressed := bytes.NewReader(payload[compressionHeaderSize:])
	reader, err := zlib.NewReader(compressed)
	if err != nil {
		return nil, fmt.Errorf("%w: zlib stream: %v", ErrInvalidCompressedPayload, err)
	}
	expanded, readErr := io.ReadAll(io.LimitReader(reader, int64(maxSize)+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("%w: zlib stream: %v", ErrInvalidCompressedPayload, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("%w: close zlib stream: %v", ErrInvalidCompressedPayload, closeErr)
	}
	if len(expanded) > maxSize {
		return nil, fmt.Errorf("%w: decompressed output exceeds limit %d", ErrCompressedPayloadTooLarge, maxSize)
	}
	if compressed.Len() != 0 {
		return nil, fmt.Errorf("%w: compressed stream contains trailing data", ErrInvalidCompressedPayload)
	}
	if uint64(len(expanded)) != uncompressedLen {
		return nil, fmt.Errorf("%w: decompressed length %d does not match header %d", ErrInvalidCompressedPayload, len(expanded), uncompressedLen)
	}
	return expanded, nil
}

func validateCompressionLimit(maxSize int) error {
	if maxSize <= 0 || maxSize >= int(^uint(0)>>1) {
		return fmt.Errorf("%w: invalid maximum size %d", ErrInvalidCompressedPayload, maxSize)
	}
	return nil
}
