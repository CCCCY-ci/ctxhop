package main

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

const deviceManagementTimeout = 15 * time.Second

type deviceListReport struct {
	Scope   string           `json:"scope"`
	Devices []deviceListItem `json:"devices"`
}

type deviceListItem struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	System       string     `json:"system"`
	LastActiveAt *time.Time `json:"lastActiveAt,omitempty"`
	Local        bool       `json:"local"`
}

func runDeviceWithIO(args []string, output io.Writer) error {
	return runDeviceWithStreams(args, strings.NewReader(""), output, output)
}

func runDeviceWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	if output == nil {
		return errors.New("device: output is required")
	}
	options, err := parseDeviceOptions(args)
	if err != nil {
		return err
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}

	switch options.action {
	case deviceActionInvite:
		ctx, cancel := context.WithTimeout(context.Background(), deviceManagementTimeout)
		defer cancel()
		access, err := openAuthorizedDomain(ctx, c, configDir, "device invite")
		if err != nil {
			return err
		}
		access.close()
		invite, err := createDeviceInvite(c, configDir)
		if err != nil {
			return err
		}
		return writeDeviceInvite(output, options.output, invite)
	case deviceActionStatus:
		report, err := collectDeviceStatus(c)
		if err != nil {
			return err
		}
		if options.json {
			return writeDeviceStatusJSON(output, report)
		}
		return writeDeviceStatusText(output, report)
	case deviceActionMode:
		if err := saveDeviceMode(configDir, c, options.mode); err != nil {
			return err
		}
		_, err := fmt.Fprintf(output, "device mode: %s\n", options.mode)
		return err
	case deviceActionList:
		if input == nil {
			return errors.New("device list: input is required")
		}
		if prompt == nil {
			return errors.New("device list: prompt output is required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), deviceManagementTimeout)
		defer cancel()
		report, err := collectDeviceList(ctx, c, configDir, input, prompt)
		if err != nil {
			return err
		}
		if options.json {
			return writeDeviceListJSON(output, report)
		}
		return writeDeviceListText(output, report)
	case deviceActionRename:
		ctx, cancel := context.WithTimeout(context.Background(), deviceManagementTimeout)
		defer cancel()
		remoteUpdated, err := renameDevice(ctx, c, configDir, options.name)
		if err != nil {
			return err
		}
		if remoteUpdated {
			_, err = fmt.Fprintf(output, "device name: %s (remote updated)\n", safeListText(options.name))
		} else {
			_, err = fmt.Fprintf(output, "device name: %s (local only; remote publication skipped)\n", safeListText(options.name))
		}
		return err
	case deviceActionRotateKey:
		if input == nil {
			return errors.New("device rotate-key: input is required")
		}
		if prompt == nil {
			return errors.New("device rotate-key: prompt output is required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), deviceManagementTimeout)
		defer cancel()
		result, err := rotateDeviceKey(ctx, c, configDir, "", input, prompt, "device rotate-key")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "device key rotated: generation=%d\n", result.Generation)
		return err
	case deviceActionRemove:
		if input == nil {
			return errors.New("device remove: input is required")
		}
		if prompt == nil {
			return errors.New("device remove: prompt output is required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), deviceManagementTimeout)
		defer cancel()
		result, err := removeDevice(ctx, c, configDir, options.target, options.yes, asBufferedReader(input), prompt)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "device removed: id=%s objects=%d generation=%d\n", options.target, result.Removed, result.Generation)
		return err
	default:
		return fmt.Errorf("device: unsupported action %q", options.action)
	}
}

func collectDeviceList(ctx context.Context, c *config.Config, configDir string, input io.Reader, prompt io.Writer) (deviceListReport, error) {
	if c == nil {
		return deviceListReport{}, errors.New("device list: configuration is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return deviceListReport{}, fmt.Errorf("device list: %w", err)
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return deviceListReport{}, fmt.Errorf("device list: local device identity is invalid: %w", err)
	}

	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "device list")
	if err != nil {
		return deviceListReport{}, err
	}
	defer access.close()
	store := access.Store
	identities := access.Identities

	records, err := syncer.FetchDeviceRecordsWithIdentities(ctx, store, identities)
	if err != nil {
		return deviceListReport{}, fmt.Errorf("device list: read encrypted device records: %w", err)
	}
	activities, err := syncer.DiscoverDeviceBranches(ctx, store)
	if err != nil {
		return deviceListReport{}, fmt.Errorf("device list: discover legacy device branches: %w", err)
	}
	return mergeDeviceList(c.Device.ID, records, activities), nil
}

func mergeDeviceList(localDeviceID string, records []syncer.DeviceRecord, activities []syncer.DeviceActivity) deviceListReport {
	items := make(map[string]deviceListItem, len(records)+len(activities))
	for _, record := range records {
		item := deviceListItem{
			ID:     record.DeviceID,
			Name:   safeListText(record.Name),
			System: safeListText(record.System),
			Local:  record.DeviceID == localDeviceID,
		}
		item.LastActiveAt = deviceTimePointer(record.LastActiveAt)
		items[record.DeviceID] = item
	}
	for _, activity := range activities {
		item, exists := items[activity.DeviceID]
		if !exists {
			item = deviceListItem{
				ID:     activity.DeviceID,
				Name:   "unknown",
				System: "unknown",
				Local:  activity.DeviceID == localDeviceID,
			}
		}
		if item.LastActiveAt == nil && !activity.LastActivityAt.IsZero() {
			item.LastActiveAt = deviceTimePointer(activity.LastActivityAt)
		}
		items[activity.DeviceID] = item
	}

	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	devices := make([]deviceListItem, 0, len(ids))
	for _, id := range ids {
		item := items[id]
		if item.Name == "" {
			item.Name = "unknown"
		}
		if item.System == "" {
			item.System = "unknown"
		}
		devices = append(devices, item)
	}
	return deviceListReport{Scope: "remote", Devices: devices}
}

func deviceTimePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}

func writeDeviceListJSON(w io.Writer, report deviceListReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeDeviceListText(w io.Writer, report deviceListReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "devices: %d\n", len(report.Devices)); err != nil {
		return err
	}
	for _, device := range report.Devices {
		lastActive := "unknown"
		if device.LastActiveAt != nil {
			lastActive = device.LastActiveAt.UTC().Format(time.RFC3339)
		}
		local := ""
		if device.Local {
			local = " local"
		}
		if _, err := fmt.Fprintf(w, "- id=%s name=%s system=%s last-active=%s%s\n",
			safeListText(device.ID), safeListText(device.Name), safeListText(device.System), lastActive, local); err != nil {
			return err
		}
	}
	return nil
}

func openDeviceRemote(ctx context.Context, c *config.Config, configDir, command string) (remote.Remote, *crypto.Keyfile, error) {
	if c == nil {
		return nil, nil, fmt.Errorf("%s: configuration is unavailable", command)
	}
	store, err := buildConfiguredRemote(c, configDir)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: configure backend: %s", command, safeBackendSetupError(err))
	}
	keyfile, err := fetchValidatedRemoteKeyfile(ctx, c, store, command)
	if err != nil {
		return nil, nil, err
	}
	return store, keyfile, nil
}

func renameDevice(ctx context.Context, c *config.Config, configDir, name string) (bool, error) {
	if c == nil {
		return false, errors.New("device rename: configuration is unavailable")
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return false, fmt.Errorf("device rename: local device identity is invalid: %w", err)
	}
	record, err := syncer.NewDeviceRecord(c.Device.ID, name, runtime.GOOS, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("device rename: %w", err)
	}

	var store remote.Remote
	var public *ecdh.PublicKey
	var access *domainAccess
	if c.Device.Mode.Effective() != config.DeviceModeDisabled {
		access, err = openAuthorizedDomain(ctx, c, configDir, "device rename")
		if err != nil {
			return false, err
		}
		defer access.close()
		store = access.Store
		public = access.Public
	}

	previous := c.Device.Name
	c.Device.Name = record.Name
	if err := c.Save(configDir); err != nil {
		c.Device.Name = previous
		return false, fmt.Errorf("device rename: save configuration: %w", err)
	}
	if c.Device.Mode.Effective() == config.DeviceModeDisabled {
		return false, nil
	}

	record.LastActiveAt = time.Now().UTC()
	if public == nil {
		return false, errors.New("device rename: active encryption identity is unavailable")
	}
	if err := syncer.PutDeviceRecord(ctx, store, public, record); err != nil {
		return false, fmt.Errorf("device rename: local name saved but remote device record could not be updated: %w", err)
	}
	return true, nil
}

func publishPushDeviceRecord(ctx context.Context, c *config.Config, store remote.Remote, public *ecdh.PublicKey) error {
	if c == nil {
		return errors.New("device record: configuration is unavailable")
	}
	name := strings.TrimSpace(c.Device.Name)
	if name == "" {
		name = "unnamed-device"
	}
	record, err := syncer.NewDeviceRecord(c.Device.ID, name, runtime.GOOS, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("device record: %w", err)
	}
	return syncer.PutDeviceRecord(ctx, store, public, record)
}

type deviceKeyRotationResult struct {
	Generation  uint64
	RecoveryKey string
	Removed     int
}

func removeDevice(ctx context.Context, c *config.Config, configDir, target string, yes bool, input io.Reader, prompt io.Writer) (deviceKeyRotationResult, error) {
	if c == nil {
		return deviceKeyRotationResult{}, errors.New("device remove: configuration is unavailable")
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return deviceKeyRotationResult{}, fmt.Errorf("device remove: local device identity is invalid: %w", err)
	}
	if err := config.ValidateDeviceID(target); err != nil {
		return deviceKeyRotationResult{}, fmt.Errorf("device remove: target device identity is invalid: %w", err)
	}
	if target == c.Device.ID {
		return deviceKeyRotationResult{}, errors.New("device remove: refusing to remove the current local device")
	}
	if input == nil {
		return deviceKeyRotationResult{}, errors.New("device remove: input is required")
	}
	if prompt == nil {
		return deviceKeyRotationResult{}, errors.New("device remove: prompt output is required")
	}
	if !yes {
		confirmed, err := confirmDeviceRemoval(input, prompt, target)
		if err != nil {
			return deviceKeyRotationResult{}, err
		}
		if !confirmed {
			return deviceKeyRotationResult{}, errors.New("device remove: cancelled")
		}
	}
	if result, handled, err := retryRevokedDeviceCleanup(ctx, c, configDir, target); handled {
		return result, err
	}
	return rotateDeviceKey(ctx, c, configDir, target, input, prompt, "device remove")
}

func retryRevokedDeviceCleanup(ctx context.Context, c *config.Config, configDir, target string) (deviceKeyRotationResult, bool, error) {
	access, err := openAuthorizedDomain(ctx, c, configDir, "device remove")
	if err != nil {
		return deviceKeyRotationResult{}, false, err
	}
	defer access.close()
	member, found := keyfileMember(access.Keyfile, target)
	if !found || member.RevokedAtGeneration == 0 {
		return deviceKeyRotationResult{}, false, nil
	}
	removed, cleanupErr := syncer.DeleteDeviceData(ctx, access.Store, target)
	result := deviceKeyRotationResult{Generation: access.Keyfile.Generation, Removed: removed}
	if cleanupErr != nil {
		return result, true, fmt.Errorf("device remove: device is already cryptographically revoked; cleanup removed %d objects before failure: %w", removed, cleanupErr)
	}
	return result, true, nil
}

func rotateDeviceKey(ctx context.Context, c *config.Config, configDir, removeDeviceID string, input io.Reader, prompt io.Writer, command string) (deviceKeyRotationResult, error) {
	if c == nil {
		return deviceKeyRotationResult{}, fmt.Errorf("%s: configuration is unavailable", command)
	}
	if input == nil {
		return deviceKeyRotationResult{}, fmt.Errorf("%s: input is required", command)
	}
	if prompt == nil {
		return deviceKeyRotationResult{}, fmt.Errorf("%s: prompt output is required", command)
	}
	store, keyfile, err := openDeviceRemote(ctx, c, configDir, command)
	if err != nil {
		return deviceKeyRotationResult{}, err
	}

	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return deviceKeyRotationResult{}, fmt.Errorf("%s: load local sync material: %w", command, err)
	}
	var devicePrivate *ecdh.PrivateKey
	if len(secrets.DevicePrivateKey) != 0 {
		devicePrivate, err = crypto.ParseDevicePrivateKey(secrets.DevicePrivateKey)
		if err != nil {
			return deviceKeyRotationResult{}, fmt.Errorf("%s: parse local device authorization: %w", command, err)
		}
	} else if keyfile.IsManaged() {
		return deviceKeyRotationResult{}, fmt.Errorf("%s: local device authorization is missing; re-initialize this device with an invitation", command)
	} else {
		devicePrivate, err = crypto.NewDevicePrivateKey()
		if err != nil {
			return deviceKeyRotationResult{}, fmt.Errorf("%s: create local device authorization: %w", command, err)
		}
		secrets.DevicePrivateKey = append([]byte(nil), devicePrivate.Bytes()...)
	}

	secretInput := newCommandSecretReader(input)
	currentPassphrase, err := secretInput.read(command, prompt, "Current encryption password: ")
	if err != nil {
		return deviceKeyRotationResult{}, err
	}
	if !keyfile.IsManaged() {
		if err := config.ValidateDeviceID(c.Device.ID); err != nil {
			return deviceKeyRotationResult{}, fmt.Errorf("%s: local device identity is invalid: %w", command, err)
		}
		// Keep the legacy-to-managed migration in memory until the new
		// passphrase and Recovery Key have been confirmed. Publishing an
		// intermediate managed keyfile would leave a v1 local configuration
		// unable to push if the user cancelled the rotation.
		if err := crypto.MigrateKeyfile(keyfile, currentPassphrase, c.Device.ID, devicePrivate.PublicKey()); err != nil {
			return deviceKeyRotationResult{}, fmt.Errorf("%s: migrate remote keyfile to device authorization: %w", command, err)
		}
	}

	nextPassphrase, err := readNewPassphraseFromSecretReader(secretInput, prompt, command)
	if err != nil {
		return deviceKeyRotationResult{}, err
	}
	recoveryKey, err := crypto.RotateManagedKeyfile(keyfile, currentPassphrase, nextPassphrase, removeDeviceID)
	if err != nil {
		return deviceKeyRotationResult{}, fmt.Errorf("%s: rotate encryption key: %w", command, err)
	}
	if _, err := fmt.Fprintln(prompt, "New Recovery Key (save it before continuing):"); err != nil {
		return deviceKeyRotationResult{}, err
	}
	if _, err := fmt.Fprintln(prompt, recoveryKey); err != nil {
		return deviceKeyRotationResult{}, err
	}
	saved, err := readDeviceRecoveryConfirmation(secretInput.lines, prompt, command)
	if err != nil {
		return deviceKeyRotationResult{}, err
	}
	if !saved {
		return deviceKeyRotationResult{}, fmt.Errorf("%s: Recovery Key was not confirmed as saved; no rotation was published", command)
	}
	// Persist the local grant before publishing the final keyfile. If the
	// process stops after the remote write, the next command can still
	// authorize this device and accept the new generation.
	if err := config.SaveSecrets(configDir, secrets); err != nil {
		return deviceKeyRotationResult{Generation: keyfile.Generation, RecoveryKey: recoveryKey}, fmt.Errorf("%s: save local device authorization: %w", command, err)
	}
	if err := syncer.ReplaceKeyfile(ctx, store, keyfile); err != nil {
		return deviceKeyRotationResult{Generation: keyfile.Generation, RecoveryKey: recoveryKey}, fmt.Errorf("%s: publish rotated keyfile: %w", command, err)
	}
	ring, err := keyfile.UnlockKeyRingForDevice(c.Device.ID, devicePrivate)
	if err != nil {
		return deviceKeyRotationResult{Generation: keyfile.Generation, RecoveryKey: recoveryKey}, fmt.Errorf("%s: local device was not granted the new key generation: %w", command, err)
	}
	if err := acceptManagedDomainState(c, configDir, keyfile, ring, command); err != nil {
		ring.Close()
		return deviceKeyRotationResult{Generation: keyfile.Generation, RecoveryKey: recoveryKey}, err
	}
	ring.Close()

	result := deviceKeyRotationResult{Generation: keyfile.Generation, RecoveryKey: recoveryKey}
	if removeDeviceID == "" {
		return result, nil
	}
	removed, cleanupErr := syncer.DeleteDeviceData(ctx, store, removeDeviceID)
	result.Removed = removed
	if cleanupErr != nil {
		return result, fmt.Errorf("%s: cryptographic revocation completed, but remote data cleanup removed %d objects before failure: %w", command, removed, cleanupErr)
	}
	return result, nil
}

func asBufferedReader(input io.Reader) *bufio.Reader {
	if reader, ok := input.(*bufio.Reader); ok {
		return reader
	}
	return bufio.NewReader(input)
}

func readDeviceRecoveryConfirmation(input *bufio.Reader, prompt io.Writer, command string) (bool, error) {
	value, err := readCommandSecretReader(input, prompt, command, "Type 'saved' after storing the Recovery Key: ")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(value, "saved"), nil
}

func confirmDeviceRemoval(input io.Reader, prompt io.Writer, target string) (bool, error) {
	if input == nil {
		return false, errors.New("device remove: input is required")
	}
	if prompt == nil {
		return false, errors.New("device remove: prompt output is required")
	}
	if _, err := fmt.Fprintf(prompt, "Remove all remote data for device %q? [y/N]: ", target); err != nil {
		return false, err
	}
	line, err := asBufferedReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("device remove: read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
