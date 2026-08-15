package main

import (
	"bufio"
	"bytes"
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

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
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
	case deviceActionRemove:
		if !options.yes {
			if input == nil {
				return errors.New("device remove: input is required")
			}
			if prompt == nil {
				return errors.New("device remove: prompt output is required")
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), deviceManagementTimeout)
		defer cancel()
		removed, err := removeDevice(ctx, c, configDir, options.target, options.yes, input, prompt)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "device removed: id=%s objects=%d\n", options.target, removed)
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

	store, keyfile, err := openDeviceRemote(ctx, c, configDir, "device list")
	if err != nil {
		return deviceListReport{}, err
	}
	passphrase, err := readCommandPassphrase(input, prompt, "device list")
	if err != nil {
		return deviceListReport{}, err
	}
	dataKey, err := keyfile.UnlockWithPassphrase(passphrase)
	if err != nil {
		return deviceListReport{}, fmt.Errorf("device list: unlock remote keyfile: %w", err)
	}
	defer dataKey.Close()
	identity, err := dataKey.IdentityPrivate()
	if err != nil {
		return deviceListReport{}, fmt.Errorf("device list: open remote identity: %w", err)
	}

	records, err := syncer.FetchDeviceRecords(ctx, store, identity)
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
	keyfile, err := syncer.FetchKeyfile(ctx, store)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: read remote keyfile: %w", command, err)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		return nil, nil, fmt.Errorf("%s: validate remote identity: %w", command, err)
	}
	if len(c.IdentityPublic) == 0 || !bytes.Equal(public.Bytes(), c.IdentityPublic) {
		return nil, nil, fmt.Errorf("%s: remote encryption identity does not match this configuration", command)
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
	if c.Device.Mode.Effective() != config.DeviceModeDisabled {
		store, _, err = openDeviceRemote(ctx, c, configDir, "device rename")
		if err != nil {
			return false, err
		}
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
	public, err := ecdh.X25519().NewPublicKey(c.IdentityPublic)
	if err != nil {
		return false, fmt.Errorf("device rename: local encryption identity is invalid: %w", err)
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

func removeDevice(ctx context.Context, c *config.Config, configDir, target string, yes bool, input io.Reader, prompt io.Writer) (int, error) {
	if c == nil {
		return 0, errors.New("device remove: configuration is unavailable")
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return 0, fmt.Errorf("device remove: local device identity is invalid: %w", err)
	}
	if err := config.ValidateDeviceID(target); err != nil {
		return 0, fmt.Errorf("device remove: target device identity is invalid: %w", err)
	}
	if target == c.Device.ID {
		return 0, errors.New("device remove: refusing to remove the current local device")
	}
	if !yes {
		confirmed, err := confirmDeviceRemoval(input, prompt, target)
		if err != nil {
			return 0, err
		}
		if !confirmed {
			return 0, errors.New("device remove: cancelled")
		}
	}

	store, _, err := openDeviceRemote(ctx, c, configDir, "device remove")
	if err != nil {
		return 0, err
	}
	removed, err := syncer.DeleteDeviceData(ctx, store, target)
	if err != nil {
		if removed > 0 {
			return removed, fmt.Errorf("device remove: removed %d remote objects before failure: %w", removed, err)
		}
		return 0, fmt.Errorf("device remove: delete remote data: %w", err)
	}
	return removed, nil
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
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("device remove: read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
