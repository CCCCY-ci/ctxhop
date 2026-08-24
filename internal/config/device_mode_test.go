package config

import (
	"strings"
	"testing"
)

func TestDeviceModeDefaultsAndParsing(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  DeviceMode
	}{
		{name: "legacy", want: DeviceModeNormal},
		{name: "normal", input: "normal", want: DeviceModeNormal},
		{name: "trimmed push only", input: " PUSH-ONLY ", want: DeviceModePushOnly},
		{name: "disabled", input: "disabled", want: DeviceModeDisabled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mode, err := ParseDeviceMode(test.input)
			if err != nil {
				t.Fatalf("ParseDeviceMode: %v", err)
			}
			if mode != test.want {
				t.Fatalf("mode = %q, want %q", mode, test.want)
			}
			if test.input == "" && (DeviceMode("").Effective() != DeviceModeNormal) {
				t.Fatal("an omitted mode did not default to normal")
			}
		})
	}
}

func TestDeviceModeRejectsUnknownConfigValue(t *testing.T) {
	c := New()
	c.Remote.Type = "dir"
	c.Device.Mode = DeviceMode("receive-only")
	err := c.Save(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsupported device mode") {
		t.Fatalf("Save error = %v, want an unsupported-mode error", err)
	}
}
