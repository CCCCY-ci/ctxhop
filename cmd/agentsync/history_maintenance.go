package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

const (
	historyMaintenanceCleanup = "cleanup"
	historyMaintenancePrune   = "prune"
)

type historyPruneOptions struct {
	session  string
	path     string
	remoteID bool
	yes      bool
	keep     int
	before   *time.Time
}

type historyPruneVersion struct {
	version   syncer.Version
	updatedAt time.Time
}

func isHistoryMaintenance(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case historyMaintenanceCleanup, historyMaintenancePrune:
		return true
	default:
		return false
	}
}

func runHistoryMaintenanceWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	if len(args) == 0 {
		return errors.New("history: expected cleanup or prune")
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case historyMaintenanceCleanup:
		translated := append([]string{remoteActionDeleteSession}, args[1:]...)
		return runRemoteWithStreams(translated, input, output, prompt)
	case historyMaintenancePrune:
		return runHistoryPruneWithStreams(args[1:], input, output, prompt)
	default:
		return fmt.Errorf("history: unsupported maintenance action %q", args[0])
	}
}

func parseHistoryPruneOptions(args []string) (historyPruneOptions, error) {
	flags := flag.NewFlagSet("history prune", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	yes := flags.Bool("yes", false, "skip the confirmation prompt")
	remoteID := flags.Bool("remote-id", false, "treat the session argument as its opaque remote ID")
	path := flags.String("path", ".", "project directory used to derive the project ID")
	keep := flags.Int("keep", -1, "retain this many newest maximal versions")
	beforeText := flags.String("before", "", "remove versions older than this RFC3339 timestamp")
	if err := flags.Parse(args); err != nil {
		return historyPruneOptions{}, fmt.Errorf("history prune: %w", err)
	}
	if flags.NArg() != 1 {
		return historyPruneOptions{}, errors.New("history prune: expected one native session ID or remote ID")
	}
	if *keep < -1 {
		return historyPruneOptions{}, errors.New("history prune: --keep must be zero or greater")
	}
	if *keep >= 0 && strings.TrimSpace(*beforeText) != "" {
		return historyPruneOptions{}, errors.New("history prune: --keep and --before cannot be used together")
	}
	var before *time.Time
	if strings.TrimSpace(*beforeText) != "" {
		value, err := time.Parse(time.RFC3339, strings.TrimSpace(*beforeText))
		if err != nil {
			return historyPruneOptions{}, fmt.Errorf("history prune: --before must be RFC3339: %w", err)
		}
		value = value.UTC()
		before = &value
	}
	if *keep < 0 && before == nil {
		return historyPruneOptions{}, errors.New("history prune: provide exactly one of --keep or --before")
	}
	session := strings.TrimSpace(flags.Arg(0))
	if session == "" {
		return historyPruneOptions{}, errors.New("history prune: session ID cannot be empty")
	}
	return historyPruneOptions{
		session:  session,
		path:     *path,
		remoteID: *remoteID,
		yes:      *yes,
		keep:     *keep,
		before:   before,
	}, nil
}

func runHistoryPruneWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseHistoryPruneOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("history prune: input is required")
	}
	if output == nil {
		return errors.New("history prune: output is required")
	}
	if prompt == nil {
		return errors.New("history prune: prompt output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), historyTimeout)
	defer cancel()

	projectID, sessionID, err := resolveRemoteSession(ctx, configDir, options.path, options.session, options.remoteID)
	if err != nil {
		return err
	}
	store, keyfile, err := openDeviceRemote(ctx, c, configDir, "history prune")
	if err != nil {
		return err
	}
	passphrase, err := readCommandPassphrase(input, prompt, "history prune")
	if err != nil {
		return err
	}
	dataKey, err := keyfile.UnlockWithPassphrase(passphrase)
	if err != nil {
		return fmt.Errorf("history prune: unlock remote keyfile: %w", err)
	}
	defer dataKey.Close()
	identity, err := dataKey.IdentityPrivate()
	if err != nil {
		return fmt.Errorf("history prune: open remote identity: %w", err)
	}

	branches, err := syncer.FetchCompleteBranches(ctx, store, projectID, sessionID, identity)
	if err != nil {
		return safeHistoryPruneReadError(ctx, err)
	}
	metadata, err := syncer.FetchMetadata(ctx, store, projectID, sessionID, identity)
	if err != nil {
		return safeHistoryPruneReadError(ctx, err)
	}
	resolution, err := syncer.ResolveBranches(branches)
	if err != nil {
		return safeHistoryPruneReadError(ctx, err)
	}
	targets, retained := selectHistoryPruneDevices(metadata, resolution, branches, options)
	if len(targets) == 0 {
		_, err := fmt.Fprintf(output, "history prune: session=%s nothing to remove retained-versions=%d\n", sessionID, retained)
		return err
	}

	if !options.yes {
		confirmed, err := confirmHistoryPrune(input, prompt, sessionID, len(targets), retained, options)
		if err != nil {
			return err
		}
		if !confirmed {
			return errors.New("history prune: cancelled")
		}
	}

	removedObjects := 0
	removedBranches := 0
	for _, deviceID := range targets {
		removed, err := syncer.DeleteRemoteDeviceBranch(ctx, store, projectID, sessionID, deviceID)
		removedObjects += removed
		if err != nil {
			return fmt.Errorf("history prune: removed %d objects across %d complete branches before failure: %w", removedObjects, removedBranches, err)
		}
		removedBranches++
	}
	_, err = fmt.Fprintf(output, "history prune: session=%s removed-branches=%d objects=%d retained-versions=%d\n", sessionID, removedBranches, removedObjects, retained)
	return err
}

func selectHistoryPruneDevices(metadata []syncer.MetadataRef, resolution syncer.Resolution, branches []syncer.Branch, options historyPruneOptions) ([]string, int) {
	group := syncer.ProjectMetadataRef{Devices: metadata}
	versions := make([]historyPruneVersion, 0, len(resolution.Versions))
	for _, version := range resolution.Versions {
		versions = append(versions, historyPruneVersion{
			version:   version,
			updatedAt: matchingHistoryUpdate(group, version),
		})
	}

	retain := make(map[string]struct{})
	retainedVersions := 0
	if options.before != nil {
		for _, candidate := range versions {
			if candidate.updatedAt.IsZero() || !candidate.updatedAt.Before(*options.before) {
				retainedVersions++
				for _, deviceID := range candidate.version.Devices {
					retain[deviceID] = struct{}{}
				}
			}
		}
	} else {
		known := make([]historyPruneVersion, 0, len(versions))
		for _, candidate := range versions {
			if candidate.updatedAt.IsZero() {
				retainedVersions++
				for _, deviceID := range candidate.version.Devices {
					retain[deviceID] = struct{}{}
				}
				continue
			}
			known = append(known, candidate)
		}
		sort.SliceStable(known, func(i, j int) bool {
			if !known[i].updatedAt.Equal(known[j].updatedAt) {
				return known[i].updatedAt.After(known[j].updatedAt)
			}
			if len(known[i].version.Records) != len(known[j].version.Records) {
				return len(known[i].version.Records) > len(known[j].version.Records)
			}
			return bytes.Compare(known[i].version.HeadDigest[:], known[j].version.HeadDigest[:]) < 0
		})
		keepKnown := options.keep - retainedVersions
		if keepKnown < 0 {
			keepKnown = 0
		}
		if keepKnown > len(known) {
			keepKnown = len(known)
		}
		for _, candidate := range known[:keepKnown] {
			retainedVersions++
			for _, deviceID := range candidate.version.Devices {
				retain[deviceID] = struct{}{}
			}
		}
	}

	targets := make([]string, 0, len(branches))
	for _, branch := range branches {
		if _, exists := retain[branch.DeviceID]; !exists {
			targets = append(targets, branch.DeviceID)
		}
	}
	sort.Strings(targets)
	return targets, retainedVersions
}

func confirmHistoryPrune(input io.Reader, prompt io.Writer, sessionID string, branches, retained int, options historyPruneOptions) (bool, error) {
	if input == nil {
		return false, errors.New("history prune: input is required")
	}
	if prompt == nil {
		return false, errors.New("history prune: prompt output is required")
	}
	policy := fmt.Sprintf("keep=%d", options.keep)
	if options.before != nil {
		policy = "before=" + options.before.Format(time.RFC3339)
	}
	if _, err := fmt.Fprintf(prompt, "Prune remote history for session %q: remove %d device branches and retain %d maximal versions (%s)? [y/N]: ", sessionID, branches, retained, policy); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("history prune: read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func safeHistoryPruneReadError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return fmt.Errorf("history prune: %w", ctx.Err())
	}
	if errors.Is(err, syncer.ErrNoRemoteMetadata) || errors.Is(err, syncer.ErrNoRemoteBranches) {
		return errors.New("history prune: no complete remote versions are available")
	}
	return errors.New("history prune: remote session could not be read safely")
}
