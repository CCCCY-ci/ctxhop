package crypto

import (
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
)

const devicePrivateKeyLen = 32

// NewDevicePrivateKey creates the long-lived local key used to unwrap this
// device's grants. It is unrelated to the content identity derived from an
// epoch data key.
func NewDevicePrivateKey() (*ecdh.PrivateKey, error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("crypto: generate device key: %w", err)
	}
	return key, nil
}

// ParseDevicePrivateKey parses the raw private key persisted in local secrets.
func ParseDevicePrivateKey(raw []byte) (*ecdh.PrivateKey, error) {
	if len(raw) != devicePrivateKeyLen {
		return nil, errors.New("crypto: device private key must be 32 bytes")
	}
	key, err := ecdh.X25519().NewPrivateKey(append([]byte(nil), raw...))
	if err != nil {
		return nil, fmt.Errorf("crypto: parse device private key: %w", err)
	}
	return key, nil
}
