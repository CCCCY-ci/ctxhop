package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/atomicfile"
	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
)

const (
	deviceActionInvite     = "invite"
	deviceInviteVersion    = 1
	deviceInviteKind       = "agentsync-device-invite"
	deviceInviteNonceBytes = 32
	maxDeviceInviteBytes   = 32 << 10
)

var errInvalidDeviceInvite = errors.New("device invite: invalid invitation")

type deviceInvite struct {
	Version           int                `json:"version"`
	Kind              string             `json:"kind"`
	CreatedAt         string             `json:"createdAt"`
	DomainFingerprint string             `json:"domainFingerprint"`
	Generation        uint64             `json:"generation,omitempty"`
	Remote            config.Remote      `json:"remote"`
	Issuer            deviceInviteIssuer `json:"issuer"`
	Nonce             string             `json:"nonce"`
	Proof             string             `json:"proof"`
}

type deviceInviteIssuer struct {
	DeviceID string `json:"deviceId"`
	Name     string `json:"name"`
}

type deviceInvitePayload struct {
	Version           int                `json:"version"`
	Kind              string             `json:"kind"`
	CreatedAt         string             `json:"createdAt"`
	DomainFingerprint string             `json:"domainFingerprint"`
	Generation        uint64             `json:"generation,omitempty"`
	Remote            config.Remote      `json:"remote"`
	Issuer            deviceInviteIssuer `json:"issuer"`
	Nonce             string             `json:"nonce"`
}

func (i deviceInvite) payload() deviceInvitePayload {
	return deviceInvitePayload{
		Version:           i.Version,
		Kind:              i.Kind,
		CreatedAt:         i.CreatedAt,
		DomainFingerprint: i.DomainFingerprint,
		Generation:        i.Generation,
		Remote:            i.Remote,
		Issuer:            i.Issuer,
		Nonce:             i.Nonce,
	}
}

func (i deviceInvite) payloadBytes() ([]byte, error) {
	data, err := json.Marshal(i.payload())
	if err != nil {
		return nil, fmt.Errorf("device invite: encode proof payload: %w", err)
	}
	return data, nil
}

func (i deviceInvite) validate() error {
	switch {
	case i.Version > deviceInviteVersion:
		return fmt.Errorf("%w: version %d is newer than this build", errInvalidDeviceInvite, i.Version)
	case i.Version != deviceInviteVersion:
		return fmt.Errorf("%w: unsupported version %d", errInvalidDeviceInvite, i.Version)
	case i.Kind != deviceInviteKind:
		return fmt.Errorf("%w: unexpected invitation kind", errInvalidDeviceInvite)
	case strings.TrimSpace(i.CreatedAt) == "":
		return fmt.Errorf("%w: creation time is required", errInvalidDeviceInvite)
	}
	if _, err := time.Parse(time.RFC3339Nano, i.CreatedAt); err != nil {
		return fmt.Errorf("%w: creation time: %v", errInvalidDeviceInvite, err)
	}
	fingerprint, err := normalizeExpectedDomainFingerprint(i.DomainFingerprint)
	if err != nil || fingerprint == "" {
		if err == nil {
			err = errors.New("fingerprint is empty")
		}
		return fmt.Errorf("%w: domain fingerprint: %v", errInvalidDeviceInvite, err)
	}
	if fingerprint != strings.ToLower(strings.TrimSpace(i.DomainFingerprint)) {
		return fmt.Errorf("%w: domain fingerprint must use lowercase", errInvalidDeviceInvite)
	}
	if _, err := syncDomainNamespace(i.Remote); err != nil {
		return fmt.Errorf("%w: remote namespace: %v", errInvalidDeviceInvite, err)
	}
	if err := config.ValidateDeviceID(i.Issuer.DeviceID); err != nil {
		return fmt.Errorf("%w: issuer device ID: %v", errInvalidDeviceInvite, err)
	}
	if strings.TrimSpace(i.Issuer.Name) == "" {
		return fmt.Errorf("%w: issuer name is required", errInvalidDeviceInvite)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(i.Nonce)
	if err != nil || len(nonce) != deviceInviteNonceBytes {
		return fmt.Errorf("%w: nonce is malformed", errInvalidDeviceInvite)
	}
	return nil
}

func (i deviceInvite) validateSigned() error {
	if err := i.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(i.Proof) == "" {
		return fmt.Errorf("%w: proof is required", errInvalidDeviceInvite)
	}
	return nil
}

func loadDeviceInvite(path string) (*deviceInvite, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("%w: invitation path is required", errInvalidDeviceInvite)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("device invite: read %s: %w", safeListText(path), err)
	}
	if len(data) > maxDeviceInviteBytes {
		return nil, fmt.Errorf("%w: file is larger than %d bytes", errInvalidDeviceInvite, maxDeviceInviteBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var invite deviceInvite
	if err := decoder.Decode(&invite); err != nil {
		return nil, fmt.Errorf("%w: JSON: %v", errInvalidDeviceInvite, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, fmt.Errorf("%w: trailing JSON", errInvalidDeviceInvite)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data: %v", errInvalidDeviceInvite, err)
	}
	fingerprint, err := normalizeExpectedDomainFingerprint(invite.DomainFingerprint)
	if err != nil {
		return nil, fmt.Errorf("%w: domain fingerprint: %v", errInvalidDeviceInvite, err)
	}
	invite.DomainFingerprint = fingerprint
	if err := invite.validateSigned(); err != nil {
		return nil, err
	}
	return &invite, nil
}

func createDeviceInvite(c *config.Config, configDir string) (deviceInvite, error) {
	if c == nil {
		return deviceInvite{}, errors.New("device invite: configuration is unavailable")
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return deviceInvite{}, fmt.Errorf("device invite: local device identity is invalid: %w", err)
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return deviceInvite{}, fmt.Errorf("device invite: load local secrets: %w", err)
	}
	if len(secrets.IdentifierKey) == 0 {
		return deviceInvite{}, errors.New("device invite: local identifier key is unavailable")
	}
	fingerprint, err := syncDomainFingerprint(c)
	if err != nil {
		return deviceInvite{}, fmt.Errorf("device invite: derive sync domain fingerprint: %w", err)
	}
	if stored := strings.ToLower(strings.TrimSpace(c.DomainFingerprint)); stored != "" && stored != fingerprint {
		return deviceInvite{}, fmt.Errorf("device invite: %w: local configuration is bound to a different namespace", errDomainBindingMismatch)
	}
	nonce := make([]byte, deviceInviteNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return deviceInvite{}, fmt.Errorf("device invite: generate invitation nonce: %w", err)
	}
	name := strings.TrimSpace(c.Device.Name)
	if name == "" {
		name = "unnamed-device"
	}
	invite := deviceInvite{
		Version:           deviceInviteVersion,
		Kind:              deviceInviteKind,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		DomainFingerprint: fingerprint,
		Generation:        c.DomainGeneration,
		Remote:            c.Remote,
		Issuer:            deviceInviteIssuer{DeviceID: c.Device.ID, Name: name},
		Nonce:             base64.RawURLEncoding.EncodeToString(nonce),
	}
	if err := invite.validate(); err != nil {
		return deviceInvite{}, err
	}
	payload, err := invite.payloadBytes()
	if err != nil {
		return deviceInvite{}, err
	}
	invite.Proof, err = crypto.DomainInviteProof(secrets.IdentifierKey, payload)
	if err != nil {
		return deviceInvite{}, fmt.Errorf("device invite: create proof: %w", err)
	}
	return invite, nil
}

func marshalDeviceInvite(invite deviceInvite) ([]byte, error) {
	if err := invite.validateSigned(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(invite, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("device invite: encode invitation: %w", err)
	}
	return append(data, '\n'), nil
}

func writeDeviceInvite(output io.Writer, outputPath string, invite deviceInvite) error {
	if output == nil {
		return errors.New("device invite: output is required")
	}
	data, err := marshalDeviceInvite(invite)
	if err != nil {
		return err
	}
	if strings.TrimSpace(outputPath) == "" {
		_, err := output.Write(data)
		return err
	}
	if err := atomicfile.WriteBytes(outputPath, data); err != nil {
		return fmt.Errorf("device invite: write %s: %w", safeListText(outputPath), err)
	}
	_, err = fmt.Fprintf(output, "device invite: wrote %s\n", safeListText(outputPath))
	return err
}

func verifyDeviceInvite(invite *deviceInvite, c *config.Config, identifierKey []byte) error {
	if invite == nil {
		return fmt.Errorf("%w: invitation is missing", errInvalidDeviceInvite)
	}
	if err := invite.validateSigned(); err != nil {
		return err
	}
	if len(identifierKey) == 0 {
		return errors.New("device invite: identifier key is unavailable")
	}
	if c == nil {
		return errors.New("device invite: configuration is unavailable")
	}
	currentNamespace, err := syncDomainNamespace(c.Remote)
	if err != nil {
		return fmt.Errorf("device invite: derive configured namespace: %w", err)
	}
	invitedNamespace, err := syncDomainNamespace(invite.Remote)
	if err != nil {
		return fmt.Errorf("device invite: derive invitation namespace: %w", err)
	}
	if currentNamespace != invitedNamespace {
		return fmt.Errorf("%w: invitation Remote namespace differs from the configured Remote", errDomainBindingMismatch)
	}
	fingerprint, err := syncDomainFingerprint(c)
	if err != nil {
		return fmt.Errorf("device invite: derive configured fingerprint: %w", err)
	}
	if fingerprint != invite.DomainFingerprint {
		return fmt.Errorf("%w: invitation fingerprint does not match the configured Remote", errDomainBindingMismatch)
	}
	payload, err := invite.payloadBytes()
	if err != nil {
		return err
	}
	if err := crypto.VerifyDomainInviteProof(identifierKey, payload, invite.Proof); err != nil {
		return fmt.Errorf("device invite: invitation proof does not match this sync domain: %w", err)
	}
	return nil
}

func (o *initOptions) applyDeviceInvite(invite *deviceInvite) error {
	if o == nil || invite == nil {
		return fmt.Errorf("%w: initialization options or invitation is missing", errInvalidDeviceInvite)
	}
	if err := invite.validateSigned(); err != nil {
		return err
	}
	if o.backend != "" || o.path != "" || o.endpoint != "" || o.bucket != "" || o.region != "" || o.prefix != "" {
		return errors.New("init: --invite carries the Remote settings; do not combine it with backend, path, endpoint, bucket, region, or prefix flags")
	}
	o.backend = strings.ToLower(strings.TrimSpace(invite.Remote.Type))
	o.path = invite.Remote.Path
	o.endpoint = invite.Remote.Endpoint
	o.bucket = invite.Remote.Bucket
	o.region = invite.Remote.Region
	o.prefix = invite.Remote.Prefix
	if o.expectedDomainFingerprint != "" && !strings.EqualFold(o.expectedDomainFingerprint, invite.DomainFingerprint) {
		return errors.New("init: --expect-domain-fingerprint does not match the invitation")
	}
	o.expectedDomainFingerprint = invite.DomainFingerprint
	o.invite = invite
	return nil
}
