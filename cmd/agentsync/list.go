package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

const listTimeout = 15 * time.Second

type listOptions struct {
	json bool
}

type listReport struct {
	Scope    string        `json:"scope"`
	Sessions []listSession `json:"sessions"`
}

type listSession struct {
	RemoteID    string    `json:"remoteId"`
	NativeID    string    `json:"nativeId,omitempty"`
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"createdAt,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
	Local       bool      `json:"local"`
	Sources     []string  `json:"sources"`
	RecordCount uint64    `json:"recordCount,omitempty"`
}

func init() {
	for i := range commands {
		if commands[i].name == "list" {
			commands[i].run = runList
		}
	}
}

func runList(args []string) error {
	return runListWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runListWithIO(args []string, input io.Reader, output io.Writer) error {
	return runListWithStreams(args, input, output, output)
}

func runListWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseListOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("list: input is required")
	}
	if output == nil {
		return errors.New("list: output is required")
	}
	if prompt == nil {
		return errors.New("list: prompt output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	report, err := collectListWithPrompt(ctx, c, configDir, ".", input, output, prompt)
	if err != nil {
		return err
	}
	if options.json {
		return writeListJSON(output, report)
	}
	return writeListText(output, report)
}

func parseListOptions(args []string) (listOptions, error) {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return listOptions{}, fmt.Errorf("list: %w", err)
	}
	if flags.NArg() != 0 {
		return listOptions{}, fmt.Errorf("list: unexpected argument %q", flags.Arg(0))
	}
	return listOptions{json: *jsonOutput}, nil
}

func collectList(ctx context.Context, c *config.Config, configDir, projectDir string, input io.Reader, output io.Writer) (listReport, error) {
	return collectListWithPrompt(ctx, c, configDir, projectDir, input, output, output)
}

func collectListWithPrompt(ctx context.Context, c *config.Config, configDir, projectDir string, input io.Reader, output, prompt io.Writer) (listReport, error) {
	if c == nil {
		return listReport{}, errors.New("list: configuration is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return listReport{}, fmt.Errorf("list: %w", err)
	}

	if err := devicePullError("list", c); err != nil {
		return listReport{}, err
	}
	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return listReport{}, fmt.Errorf("list: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return listReport{}, fmt.Errorf("list: %s", reason)
	}

	switch projectPullMode(c, current.Identity.Value) {
	case projectModeExcluded:
		return listReport{}, errors.New("list: project is excluded from synchronization")
	case projectModePushOnly:
		return listReport{}, errors.New("list: project is configured as push-only; remote sessions are unavailable")
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return listReport{}, fmt.Errorf("list: local device identity is invalid: %w", err)
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return listReport{}, fmt.Errorf("list: load local sync material: %w", err)
	}
	projectID, err := crypto.ProjectID(secrets.IdentifierKey, current.Identity.Value)
	if err != nil {
		return listReport{}, fmt.Errorf("list: derive project identity: %w", err)
	}

	store, err := buildConfiguredRemote(c, configDir)
	if err != nil {
		return listReport{}, fmt.Errorf("list: configure backend: %s", safeBackendSetupError(err))
	}
	keyfile, err := syncer.FetchKeyfile(ctx, store)
	if err != nil {
		return listReport{}, fmt.Errorf("list: read remote keyfile: %w", err)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		return listReport{}, fmt.Errorf("list: validate remote identity: %w", err)
	}
	if !bytes.Equal(public.Bytes(), c.IdentityPublic) {
		return listReport{}, errors.New("list: remote encryption identity does not match this configuration")
	}

	passphrase, err := readListPassphrase(input, prompt)
	if err != nil {
		return listReport{}, err
	}
	dataKey, err := keyfile.UnlockWithPassphrase(passphrase)
	if err != nil {
		return listReport{}, fmt.Errorf("list: unlock remote keyfile: %w", err)
	}
	defer dataKey.Close()
	identity, err := dataKey.IdentityPrivate()
	if err != nil {
		return listReport{}, fmt.Errorf("list: open remote identity: %w", err)
	}

	remoteSessions, err := syncer.FetchProjectMetadata(ctx, store, projectID, identity)
	if err != nil && !errors.Is(err, syncer.ErrNoRemoteMetadata) {
		return listReport{}, fmt.Errorf("list: read encrypted session metadata: %w", err)
	}
	localSessions, err := discoverListSessions(current.Root)
	if err != nil {
		return listReport{}, err
	}
	return mergeListSessions(c.Device.ID, secrets.IdentifierKey, projectID, localSessions, remoteSessions), nil
}

func discoverListSessions(projectRoot string) ([]adapter.SessionRef, error) {
	home, err := adapter.DefaultHome()
	if err != nil {
		return nil, nil
	}
	refs, err := (adapter.Layout{Home: home}).DiscoverSessions(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("list: discover local sessions: %w", err)
	}
	return refs, nil
}

func mergeListSessions(localDeviceID string, identifierKey []byte, projectID string, local []adapter.SessionRef, remoteSessions []syncer.ProjectMetadataRef) listReport {
	items := make(map[string]*listSession)
	localByID := make(map[string]adapter.SessionRef, len(local))
	for _, ref := range local {
		sessionID, err := crypto.SessionID(identifierKey, projectID, ref.NativeID)
		if err != nil {
			continue
		}
		localByID[sessionID] = ref
		items[sessionID] = &listSession{
			RemoteID:  sessionID,
			NativeID:  ref.NativeID,
			Title:     safeListText(ref.Title),
			CreatedAt: ref.CreatedAt,
			UpdatedAt: ref.UpdatedAt,
			Local:     true,
			Sources:   []string{"local"},
		}
	}

	for _, remoteSession := range remoteSessions {
		item := items[remoteSession.SessionID]
		if item == nil {
			item = &listSession{RemoteID: remoteSession.SessionID, Title: "encrypted session metadata"}
			items[remoteSession.SessionID] = item
		}
		for _, device := range remoteSession.Devices {
			item.RecordCount = maxUint64(item.RecordCount, device.Metadata.RecordCount)
			item.Sources = appendUnique(item.Sources, listSource(localDeviceID, device.DeviceID))
			summary, err := syncflow.DecodeSessionSummary(device.Metadata.Payload)
			if err != nil {
				continue
			}
			if item.NativeID == "" || (item.NativeID != summary.NativeID && !item.Local) {
				item.NativeID = summary.NativeID
			}
			if item.Title == "" || item.Title == "encrypted session metadata" || summary.UpdatedAt.After(item.UpdatedAt) {
				item.Title = safeListText(summary.Title)
			}
			if item.CreatedAt.IsZero() || summary.CreatedAt.Before(item.CreatedAt) {
				item.CreatedAt = summary.CreatedAt
			}
			if summary.UpdatedAt.After(item.UpdatedAt) {
				item.UpdatedAt = summary.UpdatedAt
			}
		}
		if ref, ok := localByID[remoteSession.SessionID]; ok {
			item.Local = true
			if item.NativeID == "" {
				item.NativeID = ref.NativeID
			}
		}
	}

	out := make([]listSession, 0, len(items))
	for _, item := range items {
		sort.Strings(item.Sources)
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].RemoteID < out[j].RemoteID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return listReport{Scope: "project", Sessions: out}
}

func listSource(localDeviceID, deviceID string) string {
	if deviceID == localDeviceID {
		return "local"
	}
	return "device-" + deviceID
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func maxUint64(left, right uint64) uint64 {
	if left > right {
		return left
	}
	return right
}

func safeListText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func writeListJSON(w io.Writer, report listReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeListText(w io.Writer, report listReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "sessions: %d\n", len(report.Sessions)); err != nil {
		return err
	}
	for _, session := range report.Sessions {
		if _, err := fmt.Fprintf(w, "- %s", session.Title); err != nil {
			return err
		}
		if session.NativeID != "" {
			if _, err := fmt.Fprintf(w, " [%s]", safeListText(session.NativeID)); err != nil {
				return err
			}
		}
		if !session.UpdatedAt.IsZero() {
			if _, err := fmt.Fprintf(w, " updated=%s", session.UpdatedAt.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, " sources=%s\n", strings.Join(session.Sources, ",")); err != nil {
			return err
		}
	}
	return nil
}
