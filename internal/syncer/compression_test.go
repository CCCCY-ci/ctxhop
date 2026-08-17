package syncer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestCompressPayloadRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte(`{"role":"assistant","content":"repeated session content"}`), 512)

	encoded, err := compressPayload(payload, len(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(encoded, []byte(compressionMagic)) {
		t.Fatalf("encoded payload does not use compression wrapper: prefix %q", encoded[:min(len(encoded), len(compressionMagic))])
	}
	if len(encoded) >= len(payload) {
		t.Fatalf("compressed payload length = %d, original length = %d", len(encoded), len(payload))
	}

	decoded, err := decompressPayload(encoded, len(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatal("decompressed payload differs from original")
	}
}

func TestCompressPayloadFallsBackWhenCompressionWouldExpand(t *testing.T) {
	payload := []byte("x")

	encoded, err := compressPayload(payload, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(encoded, []byte(compressionMagic)) {
		t.Fatal("incompressible small payload unexpectedly received a compression wrapper")
	}
	if !bytes.Equal(encoded, payload) {
		t.Fatalf("fallback payload = %q, want %q", encoded, payload)
	}

	decoded, err := decompressPayload(encoded, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded fallback payload = %q, want %q", decoded, payload)
	}
}

func TestDecompressPayloadKeepsLegacyPayloadsReadable(t *testing.T) {
	legacy := []byte(`{"version":1,"records":[]}`)

	decoded, err := decompressPayload(legacy, len(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, legacy) {
		t.Fatalf("decoded legacy payload = %q, want %q", decoded, legacy)
	}

	legacy[0] = 'X'
	if decoded[0] != '{' {
		t.Fatal("decoded legacy payload aliases the input")
	}
}

func TestDecompressPayloadRejectsMalformedWrappers(t *testing.T) {
	payload := bytes.Repeat([]byte("valid compressed content "), 256)
	encoded, err := compressPayload(payload, len(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(encoded, []byte(compressionMagic)) {
		t.Fatal("test payload was not compressed")
	}

	invalidCases := map[string][]byte{
		"truncated header": append([]byte(nil), encoded[:compressionHeaderSize-1]...),
		"compressed length mismatch": func() []byte {
			wire := append([]byte(nil), encoded...)
			binary.BigEndian.PutUint64(wire[len(compressionMagic)+10:compressionHeaderSize], uint64(len(wire)-compressionHeaderSize+1))
			return wire
		}(),
		"corrupt stream": func() []byte {
			wire := append([]byte(nil), encoded...)
			wire[len(wire)-1] ^= 0xff
			return wire
		}(),
		"trailing data": func() []byte {
			wire := append(append([]byte(nil), encoded...), 0)
			binary.BigEndian.PutUint64(wire[len(compressionMagic)+10:compressionHeaderSize], uint64(len(wire)-compressionHeaderSize))
			return wire
		}(),
		"uncompressed length mismatch": func() []byte {
			wire := append([]byte(nil), encoded...)
			binary.BigEndian.PutUint64(wire[len(compressionMagic)+2:len(compressionMagic)+10], uint64(len(payload)-1))
			return wire
		}(),
	}

	for name, wire := range invalidCases {
		t.Run(name, func(t *testing.T) {
			if _, err := decompressPayload(wire, len(payload)); err == nil || !errors.Is(err, ErrInvalidCompressedPayload) {
				t.Fatalf("decompressPayload error = %v, want ErrInvalidCompressedPayload", err)
			}
		})
	}

	for name, mutate := range map[string]func([]byte){
		"version": func(wire []byte) {
			wire[len(compressionMagic)]++
		},
		"codec": func(wire []byte) {
			wire[len(compressionMagic)+1]++
		},
	} {
		t.Run("unsupported "+name, func(t *testing.T) {
			wire := append([]byte(nil), encoded...)
			mutate(wire)
			if _, err := decompressPayload(wire, len(payload)); err == nil || !errors.Is(err, ErrUnsupportedCompression) {
				t.Fatalf("decompressPayload error = %v, want ErrUnsupportedCompression", err)
			}
		})
	}
}

func TestDecompressPayloadEnforcesOutputLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("decompression output "), 1024)
	encoded, err := compressPayload(payload, len(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(encoded, []byte(compressionMagic)) {
		t.Fatal("test payload was not compressed")
	}

	wire := append([]byte(nil), encoded...)
	binary.BigEndian.PutUint64(wire[len(compressionMagic)+2:len(compressionMagic)+10], 512)
	if _, err := decompressPayload(wire, 512); err == nil || !errors.Is(err, ErrCompressedPayloadTooLarge) {
		t.Fatalf("decompressPayload error = %v, want ErrCompressedPayloadTooLarge", err)
	}
}

func TestCompressionLimitRejectsInvalidMaximum(t *testing.T) {
	if _, err := compressPayload([]byte("payload"), 0); err == nil || !errors.Is(err, ErrInvalidCompressedPayload) {
		t.Fatalf("compressPayload error = %v, want ErrInvalidCompressedPayload", err)
	}
	if _, err := decompressPayload([]byte("payload"), 0); err == nil || !errors.Is(err, ErrInvalidCompressedPayload) {
		t.Fatalf("decompressPayload error = %v, want ErrInvalidCompressedPayload", err)
	}
}
