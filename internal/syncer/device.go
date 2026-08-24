package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

const (
	devicePrefix                  = "v1/devices"
	deviceRecordVersion           = 1
	maxDeviceNameBytes            = 256
	maxDeviceSystemBytes          = 64
	maxDeviceRecordBytes          = 16 << 10
	maxEncryptedDeviceRecordBytes = maxDeviceRecordBytes + 1024
)

var (
	// ErrInvalidDeviceRecord reports a malformed or incomplete device record.
	ErrInvalidDeviceRecord = errors.New("syncer: invalid device record")

	// ErrUnsupportedDeviceRecord reports a device record format newer than this build.
	ErrUnsupportedDeviceRecord = errors.New("syncer: device record format is newer than this build")

	// ErrDuplicateDeviceRecord reports multiple objects for one device ID.
	ErrDuplicateDeviceRecord = errors.New("syncer: duplicate remote device record")

	// ErrRemoteDeviceRecordTooLarge reports an encrypted record above the read bound.
	ErrRemoteDeviceRecordTooLarge = errors.New("syncer: remote device record is too large")
)

// DeviceRecord is the encrypted self-description published by one device.
type DeviceRecord struct {
	DeviceID     string
	Name         string
	System       string
	LastActiveAt time.Time
}

// NewDeviceRecord validates and normalizes one device self-description.
func NewDeviceRecord(deviceID, name, system string, lastActiveAt time.Time) (DeviceRecord, error) {
	record := DeviceRecord{
		DeviceID:     deviceID,
		Name:         strings.TrimSpace(name),
		System:       strings.TrimSpace(system),
		LastActiveAt: lastActiveAt.UTC(),
	}
	if err := record.Validate(); err != nil {
		return DeviceRecord{}, err
	}
	return record, nil
}

// Validate checks the fields that may be persisted in a device record.
func (r DeviceRecord) Validate() error {
	if err := validateIdentifier(r.DeviceID); err != nil {
		return fmt.Errorf("%w: device ID: %v", ErrInvalidDeviceRecord, err)
	}
	if err := validateDeviceText("name", r.Name, maxDeviceNameBytes); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDeviceRecord, err)
	}
	if err := validateDeviceText("system", r.System, maxDeviceSystemBytes); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDeviceRecord, err)
	}
	if r.LastActiveAt.IsZero() {
		return fmt.Errorf("%w: last active time is required", ErrInvalidDeviceRecord)
	}
	return nil
}

// DeviceKey returns the encrypted self-description key for one device ID.
func DeviceKey(deviceID string) (string, error) {
	if err := validateIdentifier(deviceID); err != nil {
		return "", fmt.Errorf("syncer: invalid device identifier: %w", err)
	}
	return checkedKey(devicePrefix + "/" + deviceID)
}

// SealDeviceRecord encodes and encrypts a record for its exact object key.
func SealDeviceRecord(recipient *ecdh.PublicKey, objectKey string, record DeviceRecord) ([]byte, error) {
	deviceID, ok := parseDeviceRecordKey(objectKey)
	if !ok {
		return nil, fmt.Errorf("syncer: seal device record: invalid device record key")
	}
	if record.DeviceID != deviceID {
		return nil, fmt.Errorf("%w: record ID does not match object key", ErrInvalidDeviceRecord)
	}
	plaintext, err := marshalDeviceRecord(record)
	if err != nil {
		return nil, fmt.Errorf("syncer: encode device record: %w", err)
	}
	sealed, err := crypto.Encrypt(recipient, objectKey, plaintext)
	if err != nil {
		return nil, fmt.Errorf("syncer: encrypt device record: %w", err)
	}
	return sealed, nil
}

// OpenDeviceRecord decrypts and validates a record read from its exact object key.
func OpenDeviceRecord(identity *ecdh.PrivateKey, objectKey string, sealed []byte) (DeviceRecord, error) {
	deviceID, ok := parseDeviceRecordKey(objectKey)
	if !ok {
		return DeviceRecord{}, errors.New("syncer: open device record: invalid device record key")
	}
	plaintext, err := crypto.Decrypt(identity, objectKey, sealed)
	if err != nil {
		return DeviceRecord{}, fmt.Errorf("syncer: decrypt device record: %w", err)
	}
	record, err := parseDeviceRecord(deviceID, plaintext)
	if err != nil {
		return DeviceRecord{}, fmt.Errorf("syncer: parse device record: %w", err)
	}
	return record, nil
}

// PutDeviceRecord encrypts and publishes a device self-description.
func PutDeviceRecord(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, record DeviceRecord) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return errors.New("syncer: recipient key is required")
	}
	if err := record.Validate(); err != nil {
		return err
	}
	key, err := DeviceKey(record.DeviceID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: publish device record: %w", err)
	}
	sealed, err := SealDeviceRecord(recipient, key, record)
	if err != nil {
		return err
	}
	if len(sealed) > maxEncryptedDeviceRecordBytes {
		return fmt.Errorf("%w: maximum is %d bytes", ErrRemoteDeviceRecordTooLarge, maxEncryptedDeviceRecordBytes)
	}
	if err := store.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return fmt.Errorf("syncer: publish device record: %w", err)
	}
	return nil
}

// FetchDeviceRecords reads and decrypts every explicit device record.
func FetchDeviceRecords(ctx context.Context, store remote.Remote, identity *ecdh.PrivateKey) ([]DeviceRecord, error) {
	return FetchDeviceRecordsWithIdentities(ctx, store, []*ecdh.PrivateKey{identity})
}

// FetchDeviceRecordsWithIdentities reads records encrypted under any retained
// content-key generation.
func FetchDeviceRecordsWithIdentities(ctx context.Context, store remote.Remote, identities []*ecdh.PrivateKey) ([]DeviceRecord, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	objects, err := store.List(ctx, devicePrefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list remote device records: %w", err)
	}
	keys, err := collectDeviceRecordKeys(objects)
	if err != nil {
		return nil, err
	}

	records := make([]DeviceRecord, 0, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("syncer: read remote device records: %w", err)
		}
		sealed, err := readRemoteDeviceRecord(ctx, store, key)
		if err != nil {
			return nil, fmt.Errorf("syncer: read remote device record: %w", err)
		}
		record, err := openDeviceRecordWithIdentities(identities, key, sealed)
		if err != nil {
			return nil, fmt.Errorf("syncer: open remote device record: %w", err)
		}
		records = append(records, record)
	}
	return records, nil
}

// DeviceActivity contains a device ID inferred from session branch keys.
type DeviceActivity struct {
	DeviceID       string
	LastActivityAt time.Time
}

// DiscoverDeviceBranches lists device IDs present in valid session branch keys.
// The timestamp is backend metadata and is advisory; it is never used to order
// or resolve session versions.
func DiscoverDeviceBranches(ctx context.Context, store remote.Remote) ([]DeviceActivity, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	objects, err := store.List(ctx, objectPrefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list remote session branches: %w", err)
	}
	latest := make(map[string]time.Time)
	for _, object := range objects {
		deviceID, ok := parseSessionBranchObjectKey(object.Key)
		if !ok {
			continue
		}
		previous, exists := latest[deviceID]
		if !exists || object.ModTime.After(previous) {
			latest[deviceID] = object.ModTime
		}
	}

	ids := make([]string, 0, len(latest))
	for deviceID := range latest {
		ids = append(ids, deviceID)
	}
	sort.Strings(ids)
	activities := make([]DeviceActivity, 0, len(ids))
	for _, deviceID := range ids {
		activities = append(activities, DeviceActivity{
			DeviceID:       deviceID,
			LastActivityAt: latest[deviceID],
		})
	}
	return activities, nil
}

// DeviceDataKeys returns the device record and session branch keys owned by a device.
func DeviceDataKeys(ctx context.Context, store remote.Remote, deviceID string) ([]string, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentifier(deviceID); err != nil {
		return nil, fmt.Errorf("syncer: invalid device identifier: %w", err)
	}

	keys := make(map[string]struct{})
	deviceObjects, err := store.List(ctx, devicePrefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list device records for cleanup: %w", err)
	}
	for _, object := range deviceObjects {
		id, ok := parseDeviceRecordKey(object.Key)
		if ok && id == deviceID {
			keys[object.Key] = struct{}{}
		}
	}

	sessionObjects, err := store.List(ctx, objectPrefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list session branches for cleanup: %w", err)
	}
	for _, object := range sessionObjects {
		if id, ok := parseSessionBranchObjectKey(object.Key); ok && id == deviceID {
			keys[object.Key] = struct{}{}
		}
		if _, id, ok := parseProjectAnnouncementKey(object.Key); ok && id == deviceID {
			keys[object.Key] = struct{}{}
		}
	}

	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

// DeleteDeviceData removes all device-owned remote objects and returns the
// number removed. A failure after a partial cleanup returns both values.
func DeleteDeviceData(ctx context.Context, store remote.Remote, deviceID string) (int, error) {
	keys, err := DeviceDataKeys(ctx, store, deviceID)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return removed, fmt.Errorf("syncer: delete device data: %w", err)
		}
		if err := store.Delete(ctx, key); err != nil {
			return removed, fmt.Errorf("syncer: delete device data: %w", err)
		}
		removed++
	}
	return removed, nil
}

type deviceRecordWire struct {
	Version      int    `json:"version"`
	Name         string `json:"name"`
	System       string `json:"system"`
	LastActiveAt string `json:"lastActiveAt"`
}

func marshalDeviceRecord(record DeviceRecord) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	wire := deviceRecordWire{
		Version:      deviceRecordVersion,
		Name:         record.Name,
		System:       record.System,
		LastActiveAt: record.LastActiveAt.UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal device record: %w", err)
	}
	if len(data) > maxDeviceRecordBytes {
		return nil, fmt.Errorf("%w: encoded record exceeds %d bytes", ErrInvalidDeviceRecord, maxDeviceRecordBytes)
	}
	return data, nil
}

func parseDeviceRecord(deviceID string, data []byte) (DeviceRecord, error) {
	if len(data) == 0 {
		return DeviceRecord{}, fmt.Errorf("%w: empty envelope", ErrInvalidDeviceRecord)
	}
	if len(data) > maxDeviceRecordBytes {
		return DeviceRecord{}, fmt.Errorf("%w: encoded record exceeds %d bytes", ErrInvalidDeviceRecord, maxDeviceRecordBytes)
	}

	var wire deviceRecordWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return DeviceRecord{}, fmt.Errorf("%w: decode envelope: %v", ErrInvalidDeviceRecord, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return DeviceRecord{}, fmt.Errorf("%w: envelope contains trailing JSON", ErrInvalidDeviceRecord)
	} else if !errors.Is(err, io.EOF) {
		return DeviceRecord{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidDeviceRecord, err)
	}
	if wire.Version > deviceRecordVersion {
		return DeviceRecord{}, fmt.Errorf("%w: version %d", ErrUnsupportedDeviceRecord, wire.Version)
	}
	if wire.Version != deviceRecordVersion {
		return DeviceRecord{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidDeviceRecord, wire.Version)
	}
	lastActiveAt, err := time.Parse(time.RFC3339Nano, wire.LastActiveAt)
	if err != nil {
		return DeviceRecord{}, fmt.Errorf("%w: last active time: %v", ErrInvalidDeviceRecord, err)
	}
	return NewDeviceRecord(deviceID, wire.Name, wire.System, lastActiveAt)
}

func parseDeviceRecordKey(key string) (string, bool) {
	if key == "" || !strings.HasPrefix(key, devicePrefix+"/") {
		return "", false
	}
	remainder := strings.TrimPrefix(key, devicePrefix+"/")
	parts := strings.Split(remainder, "/")
	if len(parts) != 1 || validateIdentifier(parts[0]) != nil {
		return "", false
	}
	expected, err := DeviceKey(parts[0])
	if err != nil || expected != key {
		return "", false
	}
	return parts[0], true
}

func collectDeviceRecordKeys(objects []remote.ObjectInfo) ([]string, error) {
	keys := make(map[string]string)
	for _, object := range objects {
		deviceID, ok := parseDeviceRecordKey(object.Key)
		if !ok {
			continue
		}
		if existing, exists := keys[deviceID]; exists {
			return nil, fmt.Errorf("%w for %q (%s and %s)", ErrDuplicateDeviceRecord, deviceID, existing, object.Key)
		}
		keys[deviceID] = object.Key
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

func readRemoteDeviceRecord(ctx context.Context, store remote.Remote, key string) ([]byte, error) {
	reader, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get remote device record: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxEncryptedDeviceRecordBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, fmt.Errorf("read remote device record: %w (also close: %v)", readErr, closeErr)
		}
		return nil, fmt.Errorf("read remote device record: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close remote device record: %w", closeErr)
	}
	if len(data) > maxEncryptedDeviceRecordBytes {
		return nil, fmt.Errorf("%w: maximum is %d bytes", ErrRemoteDeviceRecordTooLarge, maxEncryptedDeviceRecordBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read remote device record: %w", err)
	}
	return data, nil
}

func parseSessionBranchObjectKey(key string) (string, bool) {
	if key == "" || !strings.HasPrefix(key, objectPrefix+"/") {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, objectPrefix+"/"), "/")
	if len(parts) != 5 || parts[1] != "sessions" {
		return "", false
	}
	if validateIdentifier(parts[0]) != nil || validateIdentifier(parts[2]) != nil || validateIdentifier(parts[3]) != nil {
		return "", false
	}
	if parts[4] != metaObjectName {
		if _, err := ParseShardNumber(parts[4]); err != nil {
			return "", false
		}
	}
	expected, err := checkedKey(objectPrefix + "/" + parts[0] + "/sessions/" + parts[2] + "/" + parts[3] + "/" + parts[4])
	if err != nil || expected != key {
		return "", false
	}
	return parts[3], true
}

func validateDeviceText(field, value string, maxBytes int) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fmt.Errorf("%s is empty", field)
	}
	if len([]byte(trimmed)) > maxBytes {
		return fmt.Errorf("%s is longer than %d bytes", field, maxBytes)
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}
