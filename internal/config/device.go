package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
)

const maxDeviceIDLength = 128

// DeviceMode controls which synchronization directions this installation may
// use. An empty value is accepted for configurations written before device
// modes existed and is interpreted as DeviceModeNormal.
type DeviceMode string

const (
	DeviceModeNormal   DeviceMode = "normal"
	DeviceModePushOnly DeviceMode = "push-only"
	DeviceModeDisabled DeviceMode = "disabled"
)

// Effective returns the behavior of a persisted device mode.
func (m DeviceMode) Effective() DeviceMode {
	if m == "" {
		return DeviceModeNormal
	}
	return m
}

// Validate checks the persisted device mode without changing legacy config
// files that omit the optional field.
func (m DeviceMode) Validate() error {
	switch m {
	case "", DeviceModeNormal, DeviceModePushOnly, DeviceModeDisabled:
		return nil
	default:
		return fmt.Errorf("config: unsupported device mode %q", string(m))
	}
}

// ParseDeviceMode normalizes the value accepted by a user-facing command.
func ParseDeviceMode(value string) (DeviceMode, error) {
	mode := DeviceMode(strings.ToLower(strings.TrimSpace(value)))
	if mode == "" {
		return DeviceModeNormal, nil
	}
	if err := mode.Validate(); err != nil {
		return "", err
	}
	return mode, nil
}

var (
	// ErrInvalidDeviceID reports a device identifier that cannot be used in a
	// remote object namespace.
	ErrInvalidDeviceID = errors.New("config: invalid device identifier")

	// ErrDeviceIdentityRequired reports an attempt to create an identifier
	// without the keyed material that makes it opaque across installations.
	ErrDeviceIdentityRequired = errors.New("config: device identity key is required")
)

// ValidateDeviceID checks the persisted opaque identifier shape.
func ValidateDeviceID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: identifier is empty", ErrInvalidDeviceID)
	}
	if len(id) > maxDeviceIDLength {
		return fmt.Errorf("%w: identifier is too long", ErrInvalidDeviceID)
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return fmt.Errorf("%w: identifier must contain only lowercase ASCII letters and digits", ErrInvalidDeviceID)
	}
	return nil
}

// GenerateDeviceID creates one opaque installation identifier without persisting
// it. Init uses this before publishing a managed keyfile so a failed migration
// does not leave a half-configured local installation.
func GenerateDeviceID(identifierKey []byte) (string, error) {
	if len(identifierKey) == 0 {
		return "", ErrDeviceIdentityRequired
	}
	localEntropy := make([]byte, 32)
	if _, err := rand.Read(localEntropy); err != nil {
		return "", fmt.Errorf("config: generate device identity: %w", err)
	}
	localIdentity := hex.EncodeToString(localEntropy)
	for i := range localEntropy {
		localEntropy[i] = 0
	}
	id, err := crypto.DeviceID(identifierKey, localIdentity)
	if err != nil {
		return "", fmt.Errorf("config: derive device identity: %w", err)
	}
	if err := ValidateDeviceID(id); err != nil {
		return "", err
	}
	return id, nil
}

// EnsureDeviceID creates and persists one opaque installation identifier.
//
// The identifier is generated once and then read from Config.Device.ID. It is
// derived from fresh local entropy through the user's keyed identifier domain,
// so it contains no hostname, username, path, or display name. Existing values
// are never silently replaced: changing one would create a new remote branch.
func EnsureDeviceID(dir string, c *Config, identifierKey []byte) (string, error) {
	if c == nil {
		return "", errors.New("config: no configuration")
	}
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("config: configuration directory is required")
	}
	if c.Device.ID != "" {
		if err := ValidateDeviceID(c.Device.ID); err != nil {
			return "", err
		}
		return c.Device.ID, nil
	}
	if len(identifierKey) == 0 {
		return "", ErrDeviceIdentityRequired
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("config: create configuration directory: %w", pathSafe(err))
	}

	id, err := GenerateDeviceID(identifierKey)
	if err != nil {
		return "", err
	}

	previous := c.Device.ID
	c.Device.ID = id
	if err := c.Save(dir); err != nil {
		c.Device.ID = previous
		return "", fmt.Errorf("config: persist device identity: %w", err)
	}
	return id, nil
}
