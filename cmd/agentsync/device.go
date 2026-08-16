package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/CCCCY-ci/agentsync/internal/config"
)

const (
	deviceActionStatus    = "status"
	deviceActionMode      = "mode"
	deviceActionList      = "list"
	deviceActionRename    = "rename"
	deviceActionRemove    = "remove"
	deviceActionRotateKey = "rotate-key"
)

type deviceOptions struct {
	action string
	json   bool
	mode   config.DeviceMode
	name   string
	target string
	yes    bool
	output string
}

type deviceStatusReport struct {
	Device deviceStatus `json:"device"`
}

type deviceStatus struct {
	Configured bool   `json:"configured"`
	Mode       string `json:"mode"`
}

func init() {
	for i := range commands {
		if commands[i].name == "device" {
			commands[i].run = runDevice
		}
	}
}

func runDevice(args []string) error {
	return runDeviceWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func parseDeviceOptions(args []string) (deviceOptions, error) {
	if len(args) == 0 {
		return deviceOptions{}, errors.New("device: expected 'status', 'mode', 'list', 'rename', 'remove', 'rotate-key', or 'invite'")
	}

	switch args[0] {
	case deviceActionInvite:
		flags := flag.NewFlagSet("device invite", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		output := flags.String("output", "", "write the invitation JSON to a file")
		if err := flags.Parse(args[1:]); err != nil {
			return deviceOptions{}, fmt.Errorf("device invite: %w", err)
		}
		if flags.NArg() != 0 {
			return deviceOptions{}, fmt.Errorf("device invite: unexpected argument %q", flags.Arg(0))
		}
		return deviceOptions{action: deviceActionInvite, output: *output}, nil
	case deviceActionStatus:
		flags := flag.NewFlagSet("device status", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return deviceOptions{}, fmt.Errorf("device status: %w", err)
		}
		if flags.NArg() != 0 {
			return deviceOptions{}, fmt.Errorf("device status: unexpected argument %q", flags.Arg(0))
		}
		return deviceOptions{action: deviceActionStatus, json: *jsonOutput}, nil
	case deviceActionMode:
		if len(args) != 2 {
			return deviceOptions{}, errors.New("device mode: expected one of normal, push-only, or disabled")
		}
		mode, err := config.ParseDeviceMode(args[1])
		if err != nil {
			return deviceOptions{}, fmt.Errorf("device mode: %w", err)
		}
		return deviceOptions{action: deviceActionMode, mode: mode}, nil
	case deviceActionList:
		flags := flag.NewFlagSet("device list", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return deviceOptions{}, fmt.Errorf("device list: %w", err)
		}
		if flags.NArg() != 0 {
			return deviceOptions{}, fmt.Errorf("device list: unexpected argument %q", flags.Arg(0))
		}
		return deviceOptions{action: deviceActionList, json: *jsonOutput}, nil
	case deviceActionRename:
		if len(args) != 2 {
			return deviceOptions{}, errors.New("device rename: expected one display name")
		}
		return deviceOptions{action: deviceActionRename, name: args[1]}, nil
	case deviceActionRotateKey:
		if len(args) != 1 {
			return deviceOptions{}, errors.New("device rotate-key: does not accept arguments")
		}
		return deviceOptions{action: deviceActionRotateKey}, nil
	case deviceActionRemove:
		flags := flag.NewFlagSet("device remove", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		yes := flags.Bool("yes", false, "skip the confirmation prompt")
		if err := flags.Parse(args[1:]); err != nil {
			return deviceOptions{}, fmt.Errorf("device remove: %w", err)
		}
		if flags.NArg() != 1 {
			return deviceOptions{}, errors.New("device remove: expected one device ID")
		}
		return deviceOptions{action: deviceActionRemove, target: flags.Arg(0), yes: *yes}, nil
	default:
		return deviceOptions{}, fmt.Errorf("device: unknown action %q; expected 'status', 'mode', 'list', 'rename', 'remove', 'rotate-key', or 'invite'", args[0])
	}
}

func collectDeviceStatus(c *config.Config) (deviceStatusReport, error) {
	if c == nil {
		return deviceStatusReport{}, errors.New("device status: configuration is unavailable")
	}

	summary := c.Summarise()
	return deviceStatusReport{
		Device: deviceStatus{
			Configured: summary.DeviceIdentified,
			Mode:       summary.DeviceMode,
		},
	}, nil
}

func saveDeviceMode(configDir string, c *config.Config, mode config.DeviceMode) error {
	if c == nil {
		return errors.New("device mode: configuration is unavailable")
	}
	if err := mode.Validate(); err != nil {
		return fmt.Errorf("device mode: %w", err)
	}

	previous := c.Device.Mode
	c.Device.Mode = mode
	if err := c.Save(configDir); err != nil {
		c.Device.Mode = previous
		return fmt.Errorf("device mode: save configuration: %w", err)
	}
	return nil
}

func writeDeviceStatusJSON(w io.Writer, report deviceStatusReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeDeviceStatusText(w io.Writer, report deviceStatusReport) error {
	if _, err := fmt.Fprintln(w, "device:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  identity: %s\n", readiness(report.Device.Configured)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "  mode: %s\n", report.Device.Mode)
	return err
}
