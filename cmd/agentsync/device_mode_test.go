package main

import (
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/config"
)

func TestDevicePullPolicyModes(t *testing.T) {
	for _, test := range []struct {
		mode       config.DeviceMode
		pullError  string
		statusMode string
	}{
		{mode: config.DeviceModeNormal},
		{
			mode:       config.DeviceModePushOnly,
			pullError:  "device is configured as push-only",
			statusMode: deviceStatusModePushOnly,
		},
		{
			mode:       config.DeviceModeDisabled,
			pullError:  "device is disabled",
			statusMode: deviceStatusModeDisabled,
		},
	} {
		c := config.New()
		c.Device.Mode = test.mode
		if got := deviceStatusPullMode(c); got != test.statusMode {
			t.Errorf("mode %q status mode = %q, want %q", test.mode, got, test.statusMode)
		}
		err := devicePullError("list", c)
		if test.pullError == "" {
			if err != nil {
				t.Errorf("normal device pull error = %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.pullError) {
			t.Errorf("mode %q pull error = %v", test.mode, err)
		}
	}
}

func TestDisabledDeviceSkipsPushBeforeLocalValidation(t *testing.T) {
	c := config.New()
	c.Device.Mode = config.DeviceModeDisabled
	summary, err := collectPush(t.Context(), c, t.TempDir(), t.TempDir(), pushOptions{})
	if err != nil {
		t.Fatalf("collectPush: %v", err)
	}
	if summary != (pushSummary{Skipped: 1}) {
		t.Fatalf("summary = %+v, want one skipped session", summary)
	}
}

func TestDevicePullBoundariesStopBeforeRemoteInputs(t *testing.T) {
	c := config.New()
	c.Device.Mode = config.DeviceModePushOnly
	if _, err := collectList(t.Context(), c, "", "", nil, nil); err == nil || !strings.Contains(err.Error(), "push-only") {
		t.Errorf("collectList error = %v", err)
	}
	if _, err := collectResume(t.Context(), c, "", "", resumeOptions{}, nil, nil); err == nil || !strings.Contains(err.Error(), "push-only") {
		t.Errorf("collectResume error = %v", err)
	}
	checked, err := collectRemoteStatus(t.Context(), c, "", "", nil, nil)
	if err != nil {
		t.Fatalf("collectRemoteStatus: %v", err)
	}
	if checked.Mode != deviceStatusModePushOnly {
		t.Fatalf("remote status mode = %q, want %q", checked.Mode, deviceStatusModePushOnly)
	}
}

func TestParseInitDeviceMode(t *testing.T) {
	options, err := parseInitOptions([]string{"--device-mode", " PUSH-ONLY "})
	if err != nil {
		t.Fatalf("parseInitOptions: %v", err)
	}
	if options.deviceMode != string(config.DeviceModePushOnly) {
		t.Fatalf("device mode = %q, want %q", options.deviceMode, config.DeviceModePushOnly)
	}
	if _, err := parseInitOptions([]string{"--device-mode", "receive-only"}); err == nil {
		t.Fatal("an unknown device mode was accepted")
	}
}
