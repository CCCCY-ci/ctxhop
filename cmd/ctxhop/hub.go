package main

import (
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

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

const hubCommandTimeout = 30 * time.Second

const (
	hubActionCreate = "create"
	hubActionList   = "list"
	hubActionUse    = "use"
)

type hubOptions struct {
	action string
	name   string
	json   bool
}

type hubListReport struct {
	Current string         `json:"current"`
	Hubs    []hubListEntry `json:"hubs"`
}

type hubListEntry struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Lifecycle string    `json:"lifecycle"`
	CreatedAt time.Time `json:"createdAt"`
	Current   bool      `json:"current"`
	Remote    bool      `json:"remote"`
	Devices   int       `json:"devices"`
}

type hubMutationReport struct {
	Action  string `json:"action"`
	State   string `json:"state"`
	Name    string `json:"name"`
	ID      string `json:"id"`
	Current bool   `json:"current"`
}

func init() {
	for i := range commands {
		if commands[i].name == "hub" {
			commands[i].run = runHub
		}
	}
}

func runHub(args []string) error {
	return runHubWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runHubWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseHubOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("hub: input is required")
	}
	if output == nil {
		return errors.New("hub: output is required")
	}
	if prompt == nil {
		return errors.New("hub: prompt output is required")
	}
	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hubCommandTimeout)
	defer cancel()

	switch options.action {
	case hubActionCreate:
		return createHub(ctx, c, configDir, options.name, options.json, output)
	case hubActionUse:
		return useHub(c, configDir, options.name, options.json, output)
	case hubActionList:
		report, err := collectHubList(ctx, c, configDir, input, prompt)
		if err != nil {
			return err
		}
		if options.json {
			encoder := json.NewEncoder(output)
			encoder.SetIndent("", "  ")
			return encoder.Encode(report)
		}
		return writeHubListText(output, report)
	default:
		return fmt.Errorf("hub: unsupported action %q", options.action)
	}
}

func parseHubOptions(args []string) (hubOptions, error) {
	if len(args) == 0 {
		return hubOptions{}, errors.New("hub: expected create, list, or use")
	}
	options := hubOptions{action: strings.TrimSpace(args[0])}
	if options.action != hubActionCreate && options.action != hubActionList && options.action != hubActionUse {
		return hubOptions{}, fmt.Errorf("hub: unknown action %q; expected create, list, or use", options.action)
	}
	flags := flag.NewFlagSet("hub "+options.action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&options.json, "json", false, "write machine-readable JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return hubOptions{}, fmt.Errorf("hub %s: %w", options.action, err)
	}
	if options.action == hubActionList {
		if flags.NArg() != 0 {
			return hubOptions{}, fmt.Errorf("hub list: unexpected argument %q", flags.Arg(0))
		}
		return options, nil
	}
	if flags.NArg() != 1 {
		return hubOptions{}, fmt.Errorf("hub %s: expected exactly one Hub name", options.action)
	}
	options.name = strings.TrimSpace(flags.Arg(0))
	if err := validateHubName(options.name); err != nil {
		return hubOptions{}, fmt.Errorf("hub %s: %w", options.action, err)
	}
	return options, nil
}

func validateHubName(name string) error {
	if name == "" || strings.ContainsRune(name, 0) {
		return errors.New("Hub name cannot be empty")
	}
	if len([]rune(name)) > 128 {
		return errors.New("Hub name is too long")
	}
	return nil
}

func loadHubRegistry(configDir string, identifierKey []byte) (sessionhub.Registry, error) {
	registry, err := sessionhub.LoadRegistry(configDir)
	if errors.Is(err, sessionhub.ErrRegistryNotFound) {
		return sessionhub.NewDefaultRegistry(identifierKey, time.Now().UTC())
	}
	if err != nil {
		return sessionhub.Registry{}, err
	}
	return registry, nil
}

func createHub(ctx context.Context, c *config.Config, configDir, name string, jsonOutput bool, output io.Writer) error {
	if c == nil {
		return errors.New("hub create: configuration is unavailable")
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return fmt.Errorf("hub create: load local sync material: %w", err)
	}
	registry, err := loadHubRegistry(configDir, secrets.IdentifierKey)
	if err != nil {
		return fmt.Errorf("hub create: load local Hub registry: %w", err)
	}
	hub, existed := registry.HubByName(name)
	if !existed {
		hub, err = registry.EnsureHub(secrets.IdentifierKey, name, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("hub create: %w", err)
		}
	}
	access, err := openAuthorizedDomain(ctx, c, configDir, "hub create")
	if err != nil {
		return err
	}
	defer access.close()
	if err := syncer.PutHubDescriptorForDevice(ctx, access.Store, access.Public, hub.Descriptor.HubID, c.Device.ID, hub.Descriptor); err != nil {
		return fmt.Errorf("hub create: publish Hub descriptor: %w", err)
	}
	if err := sessionhub.SaveRegistry(configDir, registry); err != nil {
		return fmt.Errorf("hub create: save local Hub registry: %w", err)
	}
	if output == nil {
		return nil
	}
	state := "created"
	if existed {
		state = "unchanged"
	}
	if jsonOutput {
		return json.NewEncoder(output).Encode(hubMutationReport{
			Action: hubActionCreate,
			State:  state,
			Name:   safeListText(hub.Descriptor.Name),
			ID:     safeListText(hub.Descriptor.HubID),
		})
	}
	_, err = fmt.Fprintf(output, "hub %s: %s\nname: %s\nid: %s\n", hubActionCreate, state, safeListText(hub.Descriptor.Name), safeListText(hub.Descriptor.HubID))
	return err
}

func useHub(c *config.Config, configDir, selector string, jsonOutput bool, output io.Writer) error {
	if c == nil {
		return errors.New("hub use: configuration is unavailable")
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return fmt.Errorf("hub use: load local sync material: %w", err)
	}
	registry, err := loadHubRegistry(configDir, secrets.IdentifierKey)
	if err != nil {
		return fmt.Errorf("hub use: load local Hub registry: %w", err)
	}
	hub, ok := registry.HubByName(selector)
	if !ok {
		for _, candidate := range registry.Hubs {
			if candidate.Descriptor.HubID == strings.TrimSpace(selector) {
				hub = candidate
				ok = true
				break
			}
		}
	}
	if !ok {
		return fmt.Errorf("hub use: Hub %q is not known locally; create it first or run hub list", selector)
	}
	name := hub.Descriptor.Name
	if hub.Descriptor.Lifecycle != sessionhub.HubActive {
		return fmt.Errorf("hub use: Hub %q is archived", name)
	}
	if configuredSessionHub(c) == name {
		if jsonOutput {
			return json.NewEncoder(output).Encode(hubMutationReport{Action: hubActionUse, State: "unchanged", Name: safeListText(name), ID: safeListText(hub.Descriptor.HubID), Current: true})
		}
		_, err := fmt.Fprintf(output, "hub use: unchanged\ncurrent: %s\n", safeListText(name))
		return err
	}
	c.CurrentHub = name
	if err := c.Save(configDir); err != nil {
		return fmt.Errorf("hub use: save configuration: %w", err)
	}
	if jsonOutput {
		return json.NewEncoder(output).Encode(hubMutationReport{Action: hubActionUse, State: "updated", Name: safeListText(name), ID: safeListText(hub.Descriptor.HubID), Current: true})
	}
	_, err = fmt.Fprintf(output, "hub use: updated\ncurrent: %s\nid: %s\n", safeListText(name), safeListText(hub.Descriptor.HubID))
	return err
}

func collectHubList(ctx context.Context, c *config.Config, configDir string, input io.Reader, prompt io.Writer) (hubListReport, error) {
	if c == nil {
		return hubListReport{}, errors.New("hub list: configuration is unavailable")
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return hubListReport{}, fmt.Errorf("hub list: load local sync material: %w", err)
	}
	registry, err := loadHubRegistry(configDir, secrets.IdentifierKey)
	if err != nil {
		return hubListReport{}, fmt.Errorf("hub list: load local Hub registry: %w", err)
	}
	entries := make(map[string]*hubListEntry)
	for _, hub := range registry.Hubs {
		entries[hub.Descriptor.HubID] = &hubListEntry{
			ID:        hub.Descriptor.HubID,
			Name:      hub.Descriptor.Name,
			Lifecycle: string(hub.Descriptor.Lifecycle),
			CreatedAt: hub.Descriptor.CreatedAt,
			Current:   configuredSessionHub(c) == hub.Descriptor.Name,
		}
	}

	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "hub list")
	if err != nil {
		return hubListReport{}, err
	}
	defer access.close()
	remoteRefs, err := syncer.FetchHubMetadataWithDevices(ctx, access.Store, access.Identities, access.allowedDevices())
	if err != nil && !errors.Is(err, syncer.ErrNoReplicaMetadata) {
		return hubListReport{}, fmt.Errorf("hub list: read remote Hubs: %w", err)
	}
	remoteDescriptors := make(map[string]sessionhub.HubDescriptor)
	for _, ref := range remoteRefs {
		if previous, exists := remoteDescriptors[ref.Descriptor.HubID]; exists {
			if previous.Name != ref.Descriptor.Name || previous.Lifecycle != ref.Descriptor.Lifecycle {
				return hubListReport{}, fmt.Errorf("hub list: conflicting remote descriptors for Hub %q", safeListText(ref.Descriptor.HubID))
			}
			if ref.Descriptor.CreatedAt.Before(previous.CreatedAt) {
				remoteDescriptors[ref.Descriptor.HubID] = ref.Descriptor
			}
			continue
		}
		remoteDescriptors[ref.Descriptor.HubID] = ref.Descriptor
	}
	registryChanged := false
	for _, descriptor := range remoteDescriptors {
		changed, mergeErr := registry.MergeHubDescriptor(secrets.IdentifierKey, descriptor)
		if mergeErr != nil {
			return hubListReport{}, fmt.Errorf("hub list: cache remote Hub %q: %w", safeListText(descriptor.Name), mergeErr)
		}
		registryChanged = registryChanged || changed
	}
	if registryChanged {
		if err := sessionhub.SaveRegistry(configDir, registry); err != nil {
			return hubListReport{}, fmt.Errorf("hub list: cache remote Hubs: %w", err)
		}
	}
	deviceCounts := make(map[string]map[string]struct{})
	for _, ref := range remoteRefs {
		entry := entries[ref.Descriptor.HubID]
		if entry == nil {
			entry = &hubListEntry{ID: ref.Descriptor.HubID, Name: ref.Descriptor.Name, Current: configuredSessionHub(c) == ref.Descriptor.Name}
			entries[ref.Descriptor.HubID] = entry
		}
		entry.Name = ref.Descriptor.Name
		entry.Lifecycle = string(ref.Descriptor.Lifecycle)
		if entry.CreatedAt.IsZero() || ref.Descriptor.CreatedAt.Before(entry.CreatedAt) {
			entry.CreatedAt = ref.Descriptor.CreatedAt
		}
		entry.Remote = true
		if deviceCounts[ref.Descriptor.HubID] == nil {
			deviceCounts[ref.Descriptor.HubID] = make(map[string]struct{})
		}
		deviceCounts[ref.Descriptor.HubID][ref.DeviceID] = struct{}{}
	}
	for id, devices := range deviceCounts {
		if entry := entries[id]; entry != nil {
			entry.Devices = len(devices)
		}
	}
	result := hubListReport{Current: configuredSessionHub(c), Hubs: make([]hubListEntry, 0, len(entries))}
	for _, entry := range entries {
		result.Hubs = append(result.Hubs, *entry)
	}
	sort.Slice(result.Hubs, func(i, j int) bool {
		if strings.EqualFold(result.Hubs[i].Name, result.Hubs[j].Name) {
			return result.Hubs[i].ID < result.Hubs[j].ID
		}
		return strings.ToLower(result.Hubs[i].Name) < strings.ToLower(result.Hubs[j].Name)
	})
	return result, nil
}

func writeHubListText(w io.Writer, report hubListReport) error {
	if _, err := fmt.Fprintf(w, "current: %s\n", safeListText(report.Current)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "hubs: %d\n", len(report.Hubs)); err != nil {
		return err
	}
	for _, entry := range report.Hubs {
		if _, err := fmt.Fprintf(w, "- %s", safeListText(entry.Name)); err != nil {
			return err
		}
		if entry.Current {
			if _, err := fmt.Fprint(w, " [current]"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, " id=%s lifecycle=%s remote=%t devices=%d\n", safeListText(entry.ID), safeListText(entry.Lifecycle), entry.Remote, entry.Devices); err != nil {
			return err
		}
	}
	return nil
}
