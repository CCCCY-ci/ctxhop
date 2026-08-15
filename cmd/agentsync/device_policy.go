package main

import (
	"fmt"

	"github.com/CCCCY-ci/agentsync/internal/config"
)

const (
	deviceStatusModePushOnly = "device-push-only"
	deviceStatusModeDisabled = "device-disabled"
)

// configuredDeviceMode returns the effective mode while keeping legacy config
// files, which omit the field, on the normal path.
func configuredDeviceMode(c *config.Config) config.DeviceMode {
	if c == nil {
		return config.DeviceModeNormal
	}
	return c.Device.Mode.Effective()
}

// devicePullError describes the local device boundary for commands that would
// inspect or restore remote sessions.
func devicePullError(command string, c *config.Config) error {
	switch configuredDeviceMode(c) {
	case config.DeviceModePushOnly:
		return fmt.Errorf("%s: device is configured as push-only; remote sessions are unavailable", command)
	case config.DeviceModeDisabled:
		return fmt.Errorf("%s: device is disabled; remote sessions are unavailable", command)
	default:
		return nil
	}
}

// deviceStatusPullMode maps a local device boundary to the redacted status
// mode shown by an explicit remote check.
func deviceStatusPullMode(c *config.Config) string {
	switch configuredDeviceMode(c) {
	case config.DeviceModePushOnly:
		return deviceStatusModePushOnly
	case config.DeviceModeDisabled:
		return deviceStatusModeDisabled
	default:
		return ""
	}
}
