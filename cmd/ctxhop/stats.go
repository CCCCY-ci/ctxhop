package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

const statsTimeout = 2 * time.Second

type statsOptions struct {
	json bool
}

type statsReport struct {
	Scope               string     `json:"scope"`
	CrossDeviceRestores uint64     `json:"crossDeviceRestores"`
	LastRestoredAt      *time.Time `json:"lastRestoredAt,omitempty"`
}

func init() {
	for i := range commands {
		if commands[i].name == "stats" {
			commands[i].run = runStats
		}
	}
}

func runStats(args []string) error {
	return runStatsWithIO(args, os.Stdout)
}

func runStatsWithIO(args []string, output io.Writer) error {
	if output == nil {
		return errors.New("stats: output is required")
	}
	options, err := parseStatsOptions(args)
	if err != nil {
		return err
	}
	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), statsTimeout)
	defer cancel()
	report, err := collectStats(ctx, configDir)
	if err != nil {
		return err
	}
	if options.json {
		return writeStatsJSON(output, report)
	}
	return writeStatsText(output, report)
}

func parseStatsOptions(args []string) (statsOptions, error) {
	flags := flag.NewFlagSet("stats", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return statsOptions{}, fmt.Errorf("stats: %w", err)
	}
	if flags.NArg() != 0 {
		return statsOptions{}, fmt.Errorf("stats: unexpected argument %q", flags.Arg(0))
	}
	return statsOptions{json: *jsonOutput}, nil
}

func collectStats(ctx context.Context, configDir string) (statsReport, error) {
	store, err := syncer.NewRestoreStatsStore(configDir)
	if err != nil {
		return statsReport{}, fmt.Errorf("stats: configure local state: %w", err)
	}
	stats, err := store.Load(ctx)
	if err != nil {
		return statsReport{}, fmt.Errorf("stats: read local state: %w", err)
	}
	report := statsReport{
		Scope:               "local",
		CrossDeviceRestores: stats.CrossDeviceRestores,
	}
	if !stats.LastRestoredAt.IsZero() {
		last := stats.LastRestoredAt.UTC()
		report.LastRestoredAt = &last
	}
	return report, nil
}

func writeStatsJSON(w io.Writer, report statsReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeStatsText(w io.Writer, report statsReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "cross-device-restores: %d\n", report.CrossDeviceRestores); err != nil {
		return err
	}
	last := "never"
	if report.LastRestoredAt != nil {
		last = report.LastRestoredAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := fmt.Fprintf(w, "last-restored: %s\n", last)
	return err
}
