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
	deviceActionStatus = "status"
	deviceActionMode   = "mode"
)

type deviceOptions struct {
	action string
	json   bool
	mode   config.DeviceMode
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
	return runDeviceWithIO(args, os.Stdout)
}

func runDeviceWithIO(args []string, output io.Writer) error {
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
	default:
		return fmt.Errorf("device: unsupported action %q", options.action)
	}
}

func parseDeviceOptions(args []string) (deviceOptions, error) {
	if len(args) == 0 {
		return deviceOptions{}, errors.New("device: expected 'status' or 'mode'")
	}

	switch args[0] {
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
	default:
		return deviceOptions{}, fmt.Errorf("device: unknown action %q; expected 'status' or 'mode'", args[0])
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
