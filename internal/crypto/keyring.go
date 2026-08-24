package crypto

import (
	"bytes"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	keyBundleVersion    = 1
	deviceGrantVersion  = 1
	maxManagedEpochs    = 64
	maxManagedMembers   = 256
	maxDeviceGrantBytes = 16 << 10
)

// ErrManagedKeyfileRequired reports an operation that needs the managed
// device-authorized envelope rather than the legacy passphrase-only file.
var ErrManagedKeyfileRequired = errors.New("crypto: managed keyfile is required")

// ErrDeviceRevoked reports that a device no longer has a grant for the active
// key generation. Old data already copied to that device cannot be recalled,
// but it cannot unlock newly rotated data through this keyfile.
var ErrDeviceRevoked = errors.New("crypto: device is revoked or not enrolled")

// ErrInvalidDeviceGrant reports a malformed or unauthenticated device grant.
var ErrInvalidDeviceGrant = errors.New("crypto: device key grant is invalid")

// KeyfileMember is the public membership record for one local installation.
type KeyfileMember struct {
	DeviceID            string `json:"deviceId"`
	DevicePublic        []byte `json:"devicePublic"`
	RevokedAtGeneration uint64 `json:"revokedAtGeneration,omitempty"`
}

// KeyfileGrant is one encrypted epoch key for one member.
type KeyfileGrant struct {
	DeviceID string `json:"deviceId"`
	Wrapped  []byte `json:"wrapped"`
}

// KeyfileEpoch describes a content-key generation and its per-device grants.
type KeyfileEpoch struct {
	Generation     uint64         `json:"generation"`
	IdentityPublic []byte         `json:"identityPublic"`
	Grants         []KeyfileGrant `json:"grants"`
}

// KeyEpoch is one unlocked content key and its derived object identity.
type KeyEpoch struct {
	Generation      uint64
	DataKey         *DataKey
	IdentityPrivate *ecdh.PrivateKey
	IdentityPublic  *ecdh.PublicKey
}

// KeyRing is the readable key history for one authorized device. Epochs are
// sorted oldest to newest; the last epoch is always the active generation.
type KeyRing struct {
	Generation    uint64
	IdentifierKey []byte
	Epochs        []KeyEpoch
	Members       []KeyfileMember
}

// Current returns the active epoch.
func (r *KeyRing) Current() *KeyEpoch {
	if r == nil || len(r.Epochs) == 0 {
		return nil
	}
	for i := range r.Epochs {
		if r.Epochs[i].Generation == r.Generation {
			return &r.Epochs[i]
		}
	}
	return &r.Epochs[len(r.Epochs)-1]
}

// Identities returns current first, followed by historical identities.
func (r *KeyRing) Identities() []*ecdh.PrivateKey {
	if r == nil {
		return nil
	}
	identities := make([]*ecdh.PrivateKey, 0, len(r.Epochs))
	current := r.Current()
	if current != nil {
		identities = append(identities, current.IdentityPrivate)
	}
	for i := len(r.Epochs) - 1; i >= 0; i-- {
		epoch := &r.Epochs[i]
		if current != nil && epoch.Generation == current.Generation {
			continue
		}
		identities = append(identities, epoch.IdentityPrivate)
	}
	return identities
}

// ActiveDeviceIDs returns the IDs which can receive the next generation.
func (r *KeyRing) ActiveDeviceIDs() map[string]struct{} {
	active := make(map[string]struct{})
	if r == nil {
		return active
	}
	for _, member := range r.Members {
		if member.DeviceID != "" && member.RevokedAtGeneration == 0 {
			active[member.DeviceID] = struct{}{}
		}
	}
	return active
}

// Close releases every unlocked data key and clears the stable identifier key.
func (r *KeyRing) Close() {
	if r == nil {
		return
	}
	for i := range r.Epochs {
		if r.Epochs[i].DataKey != nil {
			r.Epochs[i].DataKey.Close()
		}
		r.Epochs[i].DataKey = nil
		r.Epochs[i].IdentityPrivate = nil
		r.Epochs[i].IdentityPublic = nil
	}
	zero(r.IdentifierKey)
	r.IdentifierKey = nil
	r.Epochs = nil
	r.Members = nil
}

// IsManaged reports whether this is the device-authorized format.
func (k *Keyfile) IsManaged() bool {
	return k != nil && k.Version >= managedKeyfileVersion
}

// UnlockKeyRingWithPassphrase opens all epoch keys using the current passphrase.
func (k *Keyfile) UnlockKeyRingWithPassphrase(passphrase string) (*KeyRing, error) {
	raw, err := k.unlockPassphraseMaterial(passphrase)
	if err != nil {
		return nil, err
	}
	ring, err := k.keyRingFromMaterial(raw)
	zero(raw)
	return ring, err
}

// UnlockKeyRingWithRecoveryKey opens all epoch keys using the current recovery
// key.
func (k *Keyfile) UnlockKeyRingWithRecoveryKey(recoveryText string) (*KeyRing, error) {
	raw, err := k.unlockRecoveryMaterial(recoveryText)
	if err != nil {
		return nil, err
	}
	ring, err := k.keyRingFromMaterial(raw)
	zero(raw)
	return ring, err
}

// UnlockKeyRingForDevice opens the grants issued to one local device. It
// requires the active generation grant; historical grants alone are not enough
// to authorize a device after a rotation.
func (k *Keyfile) UnlockKeyRingForDevice(deviceID string, private *ecdh.PrivateKey) (*KeyRing, error) {
	if !k.IsManaged() {
		return nil, ErrManagedKeyfileRequired
	}
	if err := validateManagedDeviceID(deviceID); err != nil {
		return nil, err
	}
	if private == nil {
		return nil, errors.New("crypto: device private key is required")
	}
	member, ok := k.member(deviceID)
	if !ok || member.RevokedAtGeneration != 0 {
		return nil, ErrDeviceRevoked
	}
	if !bytes.Equal(member.DevicePublic, private.PublicKey().Bytes()) {
		return nil, fmt.Errorf("%w: local device public key does not match its membership record", ErrDeviceRevoked)
	}

	epochs := make([]rawEpoch, 0, len(k.Epochs))
	var identifierKey []byte
	for _, epoch := range k.orderedEpochs() {
		grant, ok := grantForDevice(epoch.Grants, deviceID)
		if !ok {
			continue
		}
		plaintext, err := Decrypt(private, deviceGrantPath(deviceID, epoch.Generation), grant.Wrapped)
		if err != nil {
			return nil, fmt.Errorf("%w for generation %d: %v", ErrInvalidDeviceGrant, epoch.Generation, err)
		}
		var wire deviceGrantWire
		if err := decodeStrict(plaintext, &wire); err != nil {
			return nil, fmt.Errorf("%w for generation %d: %v", ErrInvalidDeviceGrant, epoch.Generation, err)
		}
		if wire.Version != deviceGrantVersion || wire.DeviceID != deviceID || wire.Generation != epoch.Generation || len(wire.DataKey) != keyLen || len(wire.IdentifierKey) != keyLen {
			return nil, fmt.Errorf("%w for generation %d: envelope fields are invalid", ErrInvalidDeviceGrant, epoch.Generation)
		}
		if identifierKey == nil {
			identifierKey = append([]byte(nil), wire.IdentifierKey...)
		} else if !bytes.Equal(identifierKey, wire.IdentifierKey) {
			zero(identifierKey)
			return nil, fmt.Errorf("%w: identifier key changed between generations", ErrInvalidDeviceGrant)
		}
		epochs = append(epochs, rawEpoch{Generation: wire.Generation, DataKey: append([]byte(nil), wire.DataKey...)})
	}
	if len(epochs) == 0 || !hasGeneration(epochs, k.Generation) {
		zero(identifierKey)
		return nil, ErrDeviceRevoked
	}
	ring, err := k.keyRingFromEpochs(epochs, identifierKey)
	zero(identifierKey)
	return ring, err
}

// MigrateKeyfile converts one v1 envelope to a managed v2 envelope. Existing
// passphrase and recovery wrapping are preserved byte-for-byte.
func MigrateKeyfile(k *Keyfile, passphrase, deviceID string, devicePublic *ecdh.PublicKey) error {
	if k == nil {
		return errors.New("crypto: no keyfile")
	}
	if k.IsManaged() {
		return nil
	}
	if err := validateManagedDeviceID(deviceID); err != nil {
		return err
	}
	if devicePublic == nil {
		return errors.New("crypto: device public key is required")
	}
	dataKey, err := k.UnlockWithPassphrase(passphrase)
	if err != nil {
		return err
	}
	defer dataKey.Close()
	identifierKey, err := dataKey.IdentifierKey()
	if err != nil {
		return err
	}
	defer zero(identifierKey)
	public, err := dataKey.IdentityPublic()
	if err != nil {
		return err
	}
	grant, err := makeGrant(deviceID, devicePublic, 1, dataKey.raw, identifierKey)
	if err != nil {
		return err
	}
	next := *k
	next.Version = managedKeyfileVersion
	next.Generation = 1
	next.IdentityPublic = public.Bytes()
	next.Members = []KeyfileMember{{DeviceID: deviceID, DevicePublic: devicePublic.Bytes()}}
	next.Epochs = []KeyfileEpoch{{Generation: 1, IdentityPublic: public.Bytes(), Grants: []KeyfileGrant{grant}}}
	*k = next
	return nil
}

// RegisterManagedDevice adds a new active member and grants it every retained
// epoch. It is idempotent for the same public key and refuses re-use of a
// revoked device ID.
func RegisterManagedDevice(k *Keyfile, passphrase, deviceID string, devicePublic *ecdh.PublicKey) error {
	if k == nil || !k.IsManaged() {
		return ErrManagedKeyfileRequired
	}
	if err := validateManagedDeviceID(deviceID); err != nil {
		return err
	}
	if devicePublic == nil {
		return errors.New("crypto: device public key is required")
	}
	ring, err := k.UnlockKeyRingWithPassphrase(passphrase)
	if err != nil {
		return err
	}
	defer ring.Close()

	if existing, ok := k.member(deviceID); ok {
		if existing.RevokedAtGeneration != 0 || !bytes.Equal(existing.DevicePublic, devicePublic.Bytes()) {
			return fmt.Errorf("%w: device ID is already assigned", ErrDeviceRevoked)
		}
		return nil
	}
	if len(k.Members) >= maxManagedMembers {
		return errors.New("crypto: keyfile has reached its member limit")
	}

	grants := make([][]byte, len(ring.Epochs))
	for i, epoch := range ring.Epochs {
		grant, err := makeGrant(deviceID, devicePublic, epoch.Generation, epoch.DataKey.raw, ring.IdentifierKey)
		if err != nil {
			for j := 0; j < i; j++ {
				zero(grants[j])
			}
			return err
		}
		grants[i] = grant.Wrapped
	}
	nextMembers := append([]KeyfileMember(nil), k.Members...)
	nextMembers = append(nextMembers, KeyfileMember{DeviceID: deviceID, DevicePublic: devicePublic.Bytes()})
	nextEpochs := cloneEpochs(k.Epochs)
	for i := range nextEpochs {
		nextEpochs[i].Grants = append(nextEpochs[i].Grants, KeyfileGrant{DeviceID: deviceID, Wrapped: grants[i]})
	}
	k.Members = nextMembers
	k.Epochs = nextEpochs
	return nil
}

// RotateManagedKeyfile generates a fresh content key and recovery key. If
// removeDeviceID is non-empty, that member is tombstoned and receives no grant
// for the new generation. A different passphrase is mandatory because a
// removed device may know the old one.
func RotateManagedKeyfile(k *Keyfile, currentPassphrase, nextPassphrase, removeDeviceID string) (string, error) {
	if k == nil || !k.IsManaged() {
		return "", ErrManagedKeyfileRequired
	}
	if strings.TrimSpace(nextPassphrase) == "" {
		return "", errors.New("crypto: new passphrase is required")
	}
	if currentPassphrase == nextPassphrase {
		return "", errors.New("crypto: key rotation requires a different passphrase")
	}
	if removeDeviceID != "" {
		if err := validateManagedDeviceID(removeDeviceID); err != nil {
			return "", err
		}
	}
	ring, err := k.UnlockKeyRingWithPassphrase(currentPassphrase)
	if err != nil {
		return "", err
	}
	defer ring.Close()

	if removeDeviceID != "" {
		member, ok := k.member(removeDeviceID)
		if !ok || member.RevokedAtGeneration != 0 {
			return "", fmt.Errorf("crypto: device %q is not an active member", removeDeviceID)
		}
	}
	if ring.Current() == nil {
		return "", errors.New("crypto: keyfile has no current epoch")
	}
	if k.Generation == ^uint64(0) {
		return "", errors.New("crypto: key generation exhausted")
	}

	nextDataKey := NewDataKey()
	defer nextDataKey.Close()
	nextPublic, err := nextDataKey.IdentityPublic()
	if err != nil {
		return "", err
	}
	nextGeneration := k.Generation + 1

	nextMembers := cloneMembers(k.Members)
	active := make([]KeyfileMember, 0, len(nextMembers))
	for i := range nextMembers {
		if nextMembers[i].DeviceID == removeDeviceID {
			nextMembers[i].RevokedAtGeneration = nextGeneration
		}
		if nextMembers[i].RevokedAtGeneration == 0 {
			active = append(active, nextMembers[i])
		}
	}

	nextGrants := make([]KeyfileGrant, 0, len(active))
	for _, member := range active {
		public := publicKeyFromBytes(member.DevicePublic)
		grant, err := makeGrant(member.DeviceID, public, nextGeneration, nextDataKey.raw, ring.IdentifierKey)
		if err != nil {
			return "", err
		}
		nextGrants = append(nextGrants, grant)
	}

	nextEpochs := cloneEpochs(k.Epochs)
	nextEpochs = append(nextEpochs, KeyfileEpoch{
		Generation:     nextGeneration,
		IdentityPublic: nextPublic.Bytes(),
		Grants:         nextGrants,
	})
	if len(nextEpochs) > maxManagedEpochs {
		nextEpochs = nextEpochs[len(nextEpochs)-maxManagedEpochs:]
	}

	materialEpochs := make([]rawEpoch, 0, len(nextEpochs))
	for _, epoch := range nextEpochs {
		found := false
		for _, unlocked := range ring.Epochs {
			if unlocked.Generation == epoch.Generation {
				materialEpochs = append(materialEpochs, rawEpoch{Generation: epoch.Generation, DataKey: append([]byte(nil), unlocked.DataKey.raw...)})
				found = true
				break
			}
		}
		if !found && epoch.Generation == nextGeneration {
			materialEpochs = append(materialEpochs, rawEpoch{Generation: nextGeneration, DataKey: append([]byte(nil), nextDataKey.raw...)})
			found = true
		}
		if !found {
			return "", fmt.Errorf("crypto: retained epoch %d is not available for rotation", epoch.Generation)
		}
	}
	bundle, err := marshalKeyBundle(ring.IdentifierKey, materialEpochs)
	for _, epoch := range materialEpochs {
		zero(epoch.DataKey)
	}
	if err != nil {
		return "", err
	}

	recoveryRaw, recoveryText := NewRecoveryKey()
	defer zero(recoveryRaw)
	next := &Keyfile{
		Version:        managedKeyfileVersion,
		KDF:            DefaultKDFParams(),
		IdentityPublic: nextPublic.Bytes(),
		Generation:     nextGeneration,
		Members:        nextMembers,
		Epochs:         nextEpochs,
	}
	if err := next.wrapBundleWithPassphrase(bundle, nextPassphrase); err != nil {
		zero(bundle)
		return "", err
	}
	if err := next.wrapBundleWithRecoveryKey(bundle, recoveryRaw); err != nil {
		zero(bundle)
		return "", err
	}
	zero(bundle)
	*k = *next
	return recoveryText, nil
}

type rawEpoch struct {
	Generation uint64
	DataKey    []byte
}

type keyBundleWire struct {
	Version       int              `json:"version"`
	IdentifierKey []byte           `json:"identifierKey"`
	Epochs        []keyBundleEpoch `json:"epochs"`
}

type keyBundleEpoch struct {
	Generation uint64 `json:"generation"`
	DataKey    []byte `json:"dataKey"`
}

type deviceGrantWire struct {
	Version       int    `json:"version"`
	DeviceID      string `json:"deviceId"`
	Generation    uint64 `json:"generation"`
	DataKey       []byte `json:"dataKey"`
	IdentifierKey []byte `json:"identifierKey"`
}

func (k *Keyfile) keyRingFromMaterial(raw []byte) (*KeyRing, error) {
	if !k.IsManaged() {
		if len(raw) != keyLen {
			return nil, fmt.Errorf("%w: legacy key length is %d", ErrDamagedKeyfile, len(raw))
		}
		dk := &DataKey{raw: append([]byte(nil), raw...)}
		identity, err := dk.IdentityPrivate()
		if err != nil {
			dk.Close()
			return nil, err
		}
		id, err := dk.IdentifierKey()
		if err != nil {
			dk.Close()
			return nil, err
		}
		return &KeyRing{Generation: 1, IdentifierKey: id, Epochs: []KeyEpoch{{Generation: 1, DataKey: dk, IdentityPrivate: identity, IdentityPublic: identity.PublicKey()}}, Members: nil}, nil
	}
	epochs, identifier, err := parseKeyBundle(raw, k)
	if err != nil {
		return nil, err
	}
	return k.keyRingFromEpochs(epochs, identifier)
}

func (k *Keyfile) keyRingFromEpochs(epochs []rawEpoch, identifier []byte) (*KeyRing, error) {
	if len(epochs) == 0 || len(epochs) > maxManagedEpochs {
		return nil, fmt.Errorf("%w: invalid epoch count", ErrDamagedKeyfile)
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i].Generation < epochs[j].Generation })
	ring := &KeyRing{Generation: k.Generation, IdentifierKey: append([]byte(nil), identifier...), Members: cloneMembers(k.Members)}
	seen := make(map[uint64]struct{}, len(epochs))
	for _, raw := range epochs {
		if raw.Generation == 0 || len(raw.DataKey) != keyLen {
			ring.Close()
			return nil, fmt.Errorf("%w: invalid epoch %d", ErrDamagedKeyfile, raw.Generation)
		}
		if _, exists := seen[raw.Generation]; exists {
			ring.Close()
			return nil, fmt.Errorf("%w: duplicate epoch %d", ErrDamagedKeyfile, raw.Generation)
		}
		seen[raw.Generation] = struct{}{}
		dk := &DataKey{raw: append([]byte(nil), raw.DataKey...)}
		private, err := dk.IdentityPrivate()
		if err != nil {
			ring.Close()
			return nil, err
		}
		public := private.PublicKey()
		epoch, ok := k.epoch(raw.Generation)
		if ok && len(epoch.IdentityPublic) > 0 && !bytes.Equal(epoch.IdentityPublic, public.Bytes()) {
			ring.Close()
			return nil, fmt.Errorf("%w: epoch %d advertises a different identity", ErrPublicKeyMismatch, raw.Generation)
		}
		ring.Epochs = append(ring.Epochs, KeyEpoch{Generation: raw.Generation, DataKey: dk, IdentityPrivate: private, IdentityPublic: public})
	}
	current := ring.Current()
	if current == nil || current.Generation != k.Generation || !bytes.Equal(current.IdentityPublic.Bytes(), k.IdentityPublic) {
		ring.Close()
		return nil, fmt.Errorf("%w: active generation does not match the keyfile", ErrPublicKeyMismatch)
	}
	return ring, nil
}

func parseKeyBundle(raw []byte, k *Keyfile) ([]rawEpoch, []byte, error) {
	if len(raw) == keyLen {
		dk := &DataKey{raw: append([]byte(nil), raw...)}
		id, err := dk.IdentifierKey()
		dk.Close()
		if err != nil {
			return nil, nil, err
		}
		generation := k.Generation
		if generation == 0 {
			generation = 1
		}
		return []rawEpoch{{Generation: generation, DataKey: append([]byte(nil), raw...)}}, id, nil
	}
	var wire keyBundleWire
	if err := decodeStrict(raw, &wire); err != nil {
		return nil, nil, fmt.Errorf("%w: key bundle: %v", ErrDamagedKeyfile, err)
	}
	if wire.Version != keyBundleVersion || len(wire.IdentifierKey) != keyLen || len(wire.Epochs) == 0 || len(wire.Epochs) > maxManagedEpochs {
		return nil, nil, fmt.Errorf("%w: key bundle fields are invalid", ErrDamagedKeyfile)
	}
	epochs := make([]rawEpoch, 0, len(wire.Epochs))
	for _, epoch := range wire.Epochs {
		if epoch.Generation == 0 || len(epoch.DataKey) != keyLen {
			for _, item := range epochs {
				zero(item.DataKey)
			}
			return nil, nil, fmt.Errorf("%w: key bundle epoch is invalid", ErrDamagedKeyfile)
		}
		epochs = append(epochs, rawEpoch{Generation: epoch.Generation, DataKey: append([]byte(nil), epoch.DataKey...)})
	}
	return epochs, append([]byte(nil), wire.IdentifierKey...), nil
}

func marshalKeyBundle(identifier []byte, epochs []rawEpoch) ([]byte, error) {
	if len(identifier) != keyLen || len(epochs) == 0 || len(epochs) > maxManagedEpochs {
		return nil, fmt.Errorf("%w: key bundle fields are invalid", ErrDamagedKeyfile)
	}
	wire := keyBundleWire{Version: keyBundleVersion, IdentifierKey: append([]byte(nil), identifier...)}
	for _, epoch := range epochs {
		if epoch.Generation == 0 || len(epoch.DataKey) != keyLen {
			return nil, fmt.Errorf("%w: key bundle epoch is invalid", ErrDamagedKeyfile)
		}
		wire.Epochs = append(wire.Epochs, keyBundleEpoch{Generation: epoch.Generation, DataKey: append([]byte(nil), epoch.DataKey...)})
	}
	return json.Marshal(wire)
}

func makeGrant(deviceID string, public *ecdh.PublicKey, generation uint64, dataKey, identifierKey []byte) (KeyfileGrant, error) {
	if err := validateManagedDeviceID(deviceID); err != nil {
		return KeyfileGrant{}, err
	}
	if public == nil || len(dataKey) != keyLen || len(identifierKey) != keyLen || generation == 0 {
		return KeyfileGrant{}, fmt.Errorf("%w: grant fields are invalid", ErrInvalidDeviceGrant)
	}
	wire, err := json.Marshal(deviceGrantWire{Version: deviceGrantVersion, DeviceID: deviceID, Generation: generation, DataKey: append([]byte(nil), dataKey...), IdentifierKey: append([]byte(nil), identifierKey...)})
	if err != nil {
		return KeyfileGrant{}, fmt.Errorf("%w: encode grant: %v", ErrInvalidDeviceGrant, err)
	}
	if len(wire) > maxDeviceGrantBytes {
		return KeyfileGrant{}, fmt.Errorf("%w: grant is too large", ErrInvalidDeviceGrant)
	}
	wrapped, err := Encrypt(public, deviceGrantPath(deviceID, generation), wire)
	if err != nil {
		return KeyfileGrant{}, fmt.Errorf("%w: encrypt grant: %v", ErrInvalidDeviceGrant, err)
	}
	return KeyfileGrant{DeviceID: deviceID, Wrapped: wrapped}, nil
}

func deviceGrantPath(deviceID string, generation uint64) string {
	return fmt.Sprintf("v1/keyfile/device/%s/generation/%d", deviceID, generation)
}

func grantForDevice(grants []KeyfileGrant, deviceID string) (KeyfileGrant, bool) {
	for _, grant := range grants {
		if grant.DeviceID == deviceID {
			return grant, true
		}
	}
	return KeyfileGrant{}, false
}

func (k *Keyfile) member(deviceID string) (KeyfileMember, bool) {
	for _, member := range k.Members {
		if member.DeviceID == deviceID {
			return member, true
		}
	}
	return KeyfileMember{}, false
}

func (k *Keyfile) epoch(generation uint64) (KeyfileEpoch, bool) {
	for _, epoch := range k.Epochs {
		if epoch.Generation == generation {
			return epoch, true
		}
	}
	return KeyfileEpoch{}, false
}

func (k *Keyfile) orderedEpochs() []KeyfileEpoch {
	epochs := cloneEpochs(k.Epochs)
	sort.Slice(epochs, func(i, j int) bool { return epochs[i].Generation < epochs[j].Generation })
	return epochs
}

func cloneMembers(members []KeyfileMember) []KeyfileMember {
	out := make([]KeyfileMember, len(members))
	for i, member := range members {
		out[i] = member
		out[i].DevicePublic = append([]byte(nil), member.DevicePublic...)
	}
	return out
}

func cloneEpochs(epochs []KeyfileEpoch) []KeyfileEpoch {
	out := make([]KeyfileEpoch, len(epochs))
	for i, epoch := range epochs {
		out[i] = epoch
		out[i].IdentityPublic = append([]byte(nil), epoch.IdentityPublic...)
		out[i].Grants = make([]KeyfileGrant, len(epoch.Grants))
		for j, grant := range epoch.Grants {
			out[i].Grants[j] = grant
			out[i].Grants[j].Wrapped = append([]byte(nil), grant.Wrapped...)
		}
	}
	return out
}

func publicKeyFromBytes(data []byte) *ecdh.PublicKey {
	public, _ := ecdh.X25519().NewPublicKey(data)
	return public
}

func hasGeneration(epochs []rawEpoch, generation uint64) bool {
	for _, epoch := range epochs {
		if epoch.Generation == generation {
			return true
		}
	}
	return false
}

func validateManagedDeviceID(deviceID string) error {
	if strings.TrimSpace(deviceID) == "" || len(deviceID) > 128 {
		return fmt.Errorf("%w: device ID is invalid", ErrInvalidDeviceGrant)
	}
	for _, r := range deviceID {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return fmt.Errorf("%w: device ID contains an invalid character", ErrInvalidDeviceGrant)
		}
	}
	return nil
}

func (k *Keyfile) unlockPassphraseMaterial(passphrase string) ([]byte, error) {
	if err := k.check(); err != nil {
		return nil, err
	}
	if len(k.WrappedByPassphrase) == 0 {
		return nil, errors.New("crypto: this storage has no passphrase wrapping; unlock with your recovery key")
	}
	kek, err := passphraseKEK(passphrase, k.KDF)
	if err != nil {
		return nil, err
	}
	defer zero(kek)
	raw, err := unwrap(kek, wrapPathPassphrase, k.WrappedByPassphrase)
	if err != nil {
		return nil, translateUnwrapError(err, ErrWrongPassphrase)
	}
	return raw, nil
}

func (k *Keyfile) unlockRecoveryMaterial(recoveryText string) ([]byte, error) {
	if err := k.check(); err != nil {
		return nil, err
	}
	if len(k.WrappedByRecoveryKey) == 0 {
		return nil, errors.New("crypto: this storage has no recovery-key wrapping; unlock with your passphrase")
	}
	recoveryRaw, err := ParseRecoveryKey(recoveryText)
	if err != nil {
		return nil, err
	}
	defer zero(recoveryRaw)
	kek, err := recoveryKEK(recoveryRaw)
	if err != nil {
		return nil, err
	}
	defer zero(kek)
	raw, err := unwrap(kek, wrapPathRecovery, k.WrappedByRecoveryKey)
	if err != nil {
		return nil, translateUnwrapError(err, ErrWrongRecoveryKey)
	}
	return raw, nil
}

func (k *Keyfile) dataKeyFromMaterial(raw []byte) (*DataKey, error) {
	if !k.IsManaged() || len(raw) == keyLen {
		return k.verified(&DataKey{raw: raw})
	}
	epochs, _, err := parseKeyBundle(raw, k)
	if err != nil {
		zero(raw)
		return nil, err
	}
	defer func() {
		for _, epoch := range epochs {
			zero(epoch.DataKey)
		}
	}()
	for _, epoch := range epochs {
		if epoch.Generation != k.Generation {
			continue
		}
		dataKey := &DataKey{raw: append([]byte(nil), epoch.DataKey...)}
		zero(raw)
		return k.verified(dataKey)
	}
	zero(raw)
	return nil, fmt.Errorf("%w: active generation is absent", ErrDamagedKeyfile)
}

func (k *Keyfile) wrapMaterialWithPassphrase(material []byte, passphrase string, params KDFParams) ([]byte, error) {
	kek, err := passphraseKEK(passphrase, params)
	if err != nil {
		return nil, err
	}
	defer zero(kek)
	sealed, err := wrap(kek, wrapPathPassphrase, material)
	if err != nil {
		return nil, fmt.Errorf("wrap key material: %w", err)
	}
	return sealed, nil
}

func (k *Keyfile) wrapMaterialWithRecoveryKey(material, recoveryRaw []byte) ([]byte, error) {
	kek, err := recoveryKEK(recoveryRaw)
	if err != nil {
		return nil, err
	}
	defer zero(kek)
	sealed, err := wrap(kek, wrapPathRecovery, material)
	if err != nil {
		return nil, fmt.Errorf("wrap key material: %w", err)
	}
	return sealed, nil
}

func (k *Keyfile) wrapBundleWithPassphrase(bundle []byte, passphrase string) error {
	sealed, err := k.wrapMaterialWithPassphrase(bundle, passphrase, k.KDF)
	if err != nil {
		return err
	}
	k.WrappedByPassphrase = sealed
	return nil
}

func (k *Keyfile) wrapBundleWithRecoveryKey(bundle, recoveryRaw []byte) error {
	sealed, err := k.wrapMaterialWithRecoveryKey(bundle, recoveryRaw)
	if err != nil {
		return err
	}
	k.WrappedByRecoveryKey = sealed
	return nil
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
