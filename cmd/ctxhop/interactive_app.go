package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"golang.org/x/term"
)

// The interactive entrypoint is a navigation layer over the command handlers.
// It deliberately does not introduce a second implementation of sync,
// project, device, or Session Hub behavior. Every selected action below calls
// the same stream-aware handler used by the explicit command surface.
var (
	errInteractiveAppCancelled  = errors.New("ctxhop: interactive menu cancelled")
	errInteractiveMenuCancelled = errors.New("ctxhop: interactive submenu cancelled")
)

type interactiveMenuItem struct {
	id      string
	title   string
	detail  string
	action  func() error
	submenu func() error
}

// interactiveSessionSelection keeps the public logical Session selector next
// to the concrete source needed by an action. A logical Session may contain
// several Agent replicas, so routing only by SessionID is not enough for
// resume. legacyID is used only by v1 compatibility handlers that cannot
// consume a v2 logical ID.
type interactiveSessionSelection struct {
	logicalID string
	source    sessionSourceEntry
}

func isInteractiveTerminal(input io.Reader, output io.Writer) bool {
	if _, ok := terminalInput(input); !ok {
		return false
	}
	file, ok := output.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func runInteractiveApp(input io.Reader, output, prompt io.Writer) error {
	if input == nil {
		return errors.New("ctxhop: interactive mode requires input")
	}
	if output == nil {
		return errors.New("ctxhop: interactive mode requires output")
	}
	if prompt == nil {
		return errors.New("ctxhop: interactive mode requires prompt output")
	}
	if !isInteractiveTerminal(input, output) {
		return errors.New("ctxhop: interactive mode requires a terminal; use an explicit command for non-interactive use")
	}

	items := []sessionPickerItem{
		{id: "resume", title: "Resume a session", detail: "Find a saved context and continue"},
		{id: "sessions", title: "Browse sessions", detail: "Search titles, inspect history and switch Agent"},
		{id: "sync", title: "Sync current workspace", detail: "Push sessions and workspace context"},
		{id: "push", title: "Push session records", detail: "Publish Agent sessions without workspace files"},
		{id: "pull", title: "Check remote updates", detail: "Read metadata; do not restore anything"},
		{id: "projects", title: "Projects and policies", detail: "Bind projects and choose sync behavior"},
		{id: "devices", title: "Devices", detail: "Connected devices and local identity"},
		{id: "settings", title: "Settings and security", detail: "Domain, hooks and encryption"},
		{id: "diagnostics", title: "Diagnostics", detail: "Status, remote checks and repair clues"},
		{id: "remote", title: "Remote data", detail: "Review or remove synchronized records"},
		{id: "advanced", title: "Advanced tools", detail: "Restore, migrate, watch and manage installation"},
		{id: "setup", title: "Set up or reconfigure", detail: "Run the guided CtxHop setup"},
		{id: "quit", title: "Quit", detail: "Esc also quits"},
	}

	for {
		if err := clearInteractiveScreen(output); err != nil {
			return err
		}
		selected, err := runInteractivePicker(input, output, items, interactivePickerOptions{
			errorPrefix:  "ctxhop",
			heading:      "CtxHop",
			help:         "Type to search  |  Up/Down move  |  Enter open  |  Esc quit",
			itemNoun:     "action",
			emptyMessage: "No matching actions. Keep typing or press Esc.",
			cancelError:  errInteractiveAppCancelled,
		})
		if errors.Is(err, errInteractiveAppCancelled) || selected == "quit" {
			return nil
		}
		if err != nil {
			return err
		}

		switch selected {
		case "resume":
			err = runInteractiveAction(input, output, prompt, "Resume a session", func() error {
				return runSessionWithStreams([]string{sessionActionResume}, input, output, prompt)
			})
		case "sessions":
			err = runInteractiveSessionMenu(input, output, prompt)
		case "sync":
			err = runInteractiveAction(input, output, prompt, "Sync current workspace", func() error {
				return runPushWithIO([]string{"--workspace"}, output)
			})
		case "push":
			err = runInteractiveAction(input, output, prompt, "Push session records", func() error {
				return runPushWithIO(nil, output)
			})
		case "pull":
			err = runInteractiveAction(input, output, prompt, "Check remote updates", func() error {
				return runPullWithIO(nil, input, output, prompt)
			})
		case "projects":
			err = runInteractiveProjectMenu(input, output, prompt)
		case "devices":
			err = runInteractiveDeviceMenu(input, output, prompt)
		case "settings":
			err = runInteractiveSettingsMenu(input, output, prompt)
		case "diagnostics":
			err = runInteractiveDiagnosticsMenu(input, output, prompt)
		case "remote":
			err = runInteractiveRemoteMenu(input, output, prompt)
		case "advanced":
			err = runInteractiveAdvancedMenu(input, output, prompt)
		case "setup":
			err = runInteractiveAction(input, output, prompt, "Set up or reconfigure CtxHop", func() error {
				return runInitWithIO(nil, input, output, os.Args[0])
			})
		default:
			err = fmt.Errorf("ctxhop: unknown interactive action %q", selected)
		}
		if err != nil {
			return err
		}
	}
}

func runInteractiveMenu(input io.Reader, output, prompt io.Writer, heading string, items []interactiveMenuItem) error {
	pickerItems := make([]sessionPickerItem, 0, len(items))
	actions := make(map[string]func() error, len(items))
	submenus := make(map[string]func() error, len(items))
	for _, item := range items {
		pickerItems = append(pickerItems, sessionPickerItem{id: item.id, title: item.title, detail: item.detail})
		actions[item.id] = item.action
		submenus[item.id] = item.submenu
	}

	for {
		if err := clearInteractiveScreen(output); err != nil {
			return err
		}
		selected, err := runInteractivePicker(input, output, pickerItems, interactivePickerOptions{
			errorPrefix:  "ctxhop",
			heading:      heading,
			help:         "Type to search  |  Up/Down move  |  Enter open  |  Esc back",
			itemNoun:     "action",
			emptyMessage: "No matching actions. Keep typing or press Esc.",
			cancelError:  errInteractiveMenuCancelled,
		})
		if errors.Is(err, errInteractiveMenuCancelled) || selected == "back" {
			return nil
		}
		if err != nil {
			return err
		}
		if submenu := submenus[selected]; submenu != nil {
			if err := submenu(); err != nil {
				if isInteractiveCancellation(err) {
					continue
				}
				if err := showInteractiveError(input, output, heading, err); err != nil {
					return err
				}
			}
			continue
		}
		action := actions[selected]
		if action == nil {
			return fmt.Errorf("ctxhop: interactive action %q is unavailable", selected)
		}
		if err := runInteractiveAction(input, output, prompt, selectedInteractiveTitle(pickerItems, selected), action); err != nil {
			return err
		}
	}
}

func showInteractiveError(input io.Reader, output io.Writer, heading string, actionErr error) error {
	if err := clearInteractiveScreen(output); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "%s\n\nError: %v\n\nPress any key to return.", heading, actionErr); err != nil {
		return err
	}
	return waitForInteractiveKey(input)
}

func selectedInteractiveTitle(items []sessionPickerItem, selected string) string {
	for _, item := range items {
		if item.id == selected {
			return pickerItemTitle(item.title)
		}
	}
	return selected
}

func runInteractiveAction(input io.Reader, output, prompt io.Writer, heading string, action func() error) error {
	if err := clearInteractiveScreen(output); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "%s\n\n", heading); err != nil {
		return err
	}
	actionErr := action()
	if actionErr != nil {
		if isInteractiveCancellation(actionErr) {
			if _, err := fmt.Fprintln(output, "Cancelled."); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(output, "Error: %v\n", actionErr); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(output, "\nPress any key to return."); err != nil {
		return err
	}
	return waitForInteractiveKey(input)
}

func isInteractiveCancellation(err error) bool {
	return errors.Is(err, errInteractiveAppCancelled) ||
		errors.Is(err, errInteractiveMenuCancelled) ||
		errors.Is(err, errSessionPickerCancelled)
}

func clearInteractiveScreen(output io.Writer) error {
	if output == nil {
		return errors.New("ctxhop: interactive output is required")
	}
	_, err := io.WriteString(output, "\x1b[2J\x1b[H")
	return err
}

func waitForInteractiveKey(input io.Reader) error {
	file, ok := terminalInput(input)
	if !ok {
		return errors.New("ctxhop: waiting for input requires a terminal")
	}
	oldState, err := term.MakeRaw(int(file.Fd()))
	if err != nil {
		return fmt.Errorf("ctxhop: enable return prompt: %w", err)
	}
	_, readErr := readSessionPickerKey(file)
	restoreErr := term.Restore(int(file.Fd()), oldState)
	if readErr != nil {
		return fmt.Errorf("ctxhop: read return prompt: %w", readErr)
	}
	if restoreErr != nil {
		return fmt.Errorf("ctxhop: restore terminal: %w", restoreErr)
	}
	return nil
}

func runInteractiveSessionMenu(input io.Reader, output, prompt io.Writer) error {
	return runInteractiveMenu(input, output, prompt, "Sessions", []interactiveMenuItem{
		{id: "browse", title: "Browse current sessions", detail: "Search, inspect and act on a Session", submenu: func() error {
			return runInteractiveSessionBrowser(input, output, prompt)
		}},
		{id: "resume", title: "Resume a session", detail: "Search by title or Agent", action: func() error {
			return runSessionWithStreams([]string{sessionActionResume}, input, output, prompt)
		}},
		{id: "details", title: "Inspect session details", detail: "Select a session, then view its sources", action: func() error {
			selected, err := chooseInteractiveSession(input, output, prompt, "Inspect session details")
			if err != nil {
				return err
			}
			return runSessionWithStreams([]string{sessionActionShow, selected}, input, output, prompt)
		}},
		{id: "history", title: "View session history", detail: "Select a session and see recoverable versions", action: func() error {
			selected, err := chooseInteractiveSessionForHistory(input, output, prompt, "View session history")
			if err != nil {
				return err
			}
			if selected.source.ReplicaID != "" {
				return runInteractiveNativeReplicaState(input, output, prompt, selected)
			}
			return runHistoryWithStreams([]string{interactiveLegacyActionID(selected)}, input, output, prompt)
		}},
		{id: "discover", title: "Discover local sessions", detail: "Find native sessions not yet linked", action: func() error {
			return runSessionWithStreams([]string{sessionActionDiscover}, input, output, prompt)
		}},
		{id: "back", title: "Back", detail: "Return to the main menu"},
	})
}

func runInteractiveSessionBrowser(input io.Reader, output, prompt io.Writer) error {
	for {
		if err := clearInteractiveScreen(output); err != nil {
			return err
		}
		selected, err := chooseInteractiveSessionSelection(input, output, prompt, "Browse current sessions")
		if errors.Is(err, errInteractiveMenuCancelled) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := runInteractiveSessionActionsForSelection(input, output, prompt, selected); err != nil {
			return err
		}
	}
}

func runInteractiveSessionActions(input io.Reader, output, prompt io.Writer, sessionID string) error {
	selection := interactiveSessionSelection{logicalID: sessionID}
	return runInteractiveSessionActionsForSelection(input, output, prompt, selection)
}

func runInteractiveSessionActionsForSelection(input io.Reader, output, prompt io.Writer, selection interactiveSessionSelection) error {
	sessionID := selection.logicalID
	items := []interactiveMenuItem{
		{id: "resume", title: "Resume this session", detail: "Continue with its original Agent", action: func() error {
			return runInteractiveResumeSelection(input, output, prompt, selection)
		}},
		{id: "details", title: "View session details", detail: "Sources, devices and Session Hub metadata", action: func() error {
			return runSessionWithStreams([]string{sessionActionShow, sessionID}, input, output, prompt)
		}},
	}
	if selection.source.ReplicaID != "" {
		if selection.source.Agent != "" && selection.source.Agent != "unknown" && selection.source.NativeID != "" {
			items = append(items, interactiveMenuItem{id: "reconcile", title: "Check native state", detail: "Compare this source Replica with the local Agent session", action: func() error {
				return runSessionWithStreams([]string{sessionActionReconcile, "--agent", selection.source.Agent, "--native", selection.source.NativeID}, input, output, prompt)
			}})
		}
		items = append(items, interactiveMenuItem{id: "cross-agent", title: "Switch Agent", detail: "Create a new native session from this context", action: func() error {
			target, err := chooseInteractiveAgent(input, output, "Choose target Agent")
			if err != nil {
				return err
			}
			return runSessionWithStreams([]string{sessionActionSwitch, "--to", target, sessionID}, input, output, prompt)
		}})
	} else {
		items = append(items,
			interactiveMenuItem{id: "history", title: "View recoverable history", detail: "Inspect versions before choosing a restore", action: func() error {
				return runHistoryWithStreams([]string{interactiveLegacyActionID(selection)}, input, output, prompt)
			}},
			interactiveMenuItem{id: "cleanup", title: "Delete remote history", detail: "Remove every remote version immediately", action: func() error {
				return runHistoryWithStreams([]string{historyMaintenanceCleanup, interactiveLegacyActionID(selection)}, input, output, prompt)
			}},
			interactiveMenuItem{id: "workspace-preview", title: "Preview workspace restore", detail: "Review files and Git state without writing", action: func() error {
				return runWorkspaceWithStreams([]string{"preview", interactiveLegacyActionID(selection)}, input, output, prompt)
			}})
	}
	items = append(items, interactiveMenuItem{id: "back", title: "Back to sessions", detail: "Return to the Session picker"})
	return runInteractiveMenu(input, output, prompt, "Selected session", items)
}

func chooseInteractiveSession(input io.Reader, output, prompt io.Writer, heading string) (string, error) {
	selection, err := chooseInteractiveSessionSelection(input, output, prompt, heading)
	if err != nil {
		return "", err
	}
	return selection.logicalID, nil
}

func chooseInteractiveSessionSelection(input io.Reader, output, prompt io.Writer, heading string) (interactiveSessionSelection, error) {
	return chooseInteractiveSessionSelectionWith(input, output, prompt, heading, nil, interactiveDefaultSessionSource)
}

func chooseInteractiveSessionForHistory(input io.Reader, output, prompt io.Writer, heading string) (interactiveSessionSelection, error) {
	return chooseInteractiveSessionSelectionWith(input, output, prompt, heading, nil, interactiveHistorySessionSource)
}

func chooseInteractiveLegacySessionSelection(input io.Reader, output, prompt io.Writer, heading string) (interactiveSessionSelection, error) {
	return chooseInteractiveSessionSelectionWith(input, output, prompt, heading, func(entry sessionListEntry) bool {
		for _, source := range entry.Sources {
			if interactiveLegacySource(source) {
				return true
			}
		}
		return false
	}, interactiveLegacySessionSource)
}

func chooseInteractiveNativeSessionSelection(input io.Reader, output, prompt io.Writer, heading string) (interactiveSessionSelection, error) {
	return chooseInteractiveSessionSelectionWith(input, output, prompt, heading, func(entry sessionListEntry) bool {
		for _, source := range entry.Sources {
			if source.ReplicaID != "" {
				return true
			}
		}
		return false
	}, interactiveDefaultSessionSource)
}

func chooseInteractiveSessionSelectionWith(input io.Reader, output, prompt io.Writer, heading string, include func(sessionListEntry) bool, sourceSelector func([]sessionSourceEntry) sessionSourceEntry) (interactiveSessionSelection, error) {
	configDir, err := config.Dir()
	if err != nil {
		return interactiveSessionSelection{}, err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return interactiveSessionSelection{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionCommandTimeout)
	defer cancel()
	report, err := collectSessionListWithPrompt(ctx, c, configDir, ".", input, prompt)
	if err != nil {
		return interactiveSessionSelection{}, err
	}
	items := sessionPickerItemsFromReportFiltered(report, include)
	if len(items) == 0 {
		if include != nil {
			return interactiveSessionSelection{}, errors.New("session: no legacy sessions are available for this action")
		}
		return interactiveSessionSelection{}, errors.New("session: no sessions are available in the current project")
	}
	selectedID, err := runInteractivePicker(input, output, items, interactivePickerOptions{
		errorPrefix:  "session",
		heading:      heading,
		help:         "Type to search  |  Up/Down move  |  Enter select  |  Esc back",
		itemNoun:     "session",
		emptyMessage: "No matching sessions. Keep typing or press Esc.",
		cancelError:  errInteractiveMenuCancelled,
	})
	if err != nil {
		return interactiveSessionSelection{}, err
	}
	for _, entry := range report.Sessions {
		if entry.SessionID != selectedID {
			continue
		}
		return interactiveSessionSelection{
			logicalID: entry.SessionID,
			source:    sourceSelector(entry.Sources),
		}, nil
	}
	return interactiveSessionSelection{}, errors.New("session: selected session is no longer available")
}

func interactiveLegacySource(source sessionSourceEntry) bool {
	return source.ReplicaID == "" && strings.TrimSpace(source.legacyID) != ""
}

func interactiveLegacySessionSource(sources []sessionSourceEntry) sessionSourceEntry {
	for _, source := range sources {
		if interactiveLegacySource(source) {
			return source
		}
	}
	return sessionSourceEntry{}
}

func interactiveHistorySessionSource(sources []sessionSourceEntry) sessionSourceEntry {
	if source := interactiveLegacySessionSource(sources); source.legacyID != "" {
		return source
	}
	return interactiveDefaultSessionSource(sources)
}

func runInteractiveNativeReplicaState(input io.Reader, output, prompt io.Writer, selection interactiveSessionSelection) error {
	if selection.source.ReplicaID == "" {
		return errors.New("session: selected session has no native Replica")
	}
	if selection.source.Agent == "" || selection.source.Agent == "unknown" || selection.source.NativeID == "" {
		return runSessionWithStreams([]string{sessionActionShow, selection.logicalID}, input, output, prompt)
	}
	return runSessionWithStreams([]string{sessionActionReconcile, "--agent", selection.source.Agent, "--native", selection.source.NativeID}, input, output, prompt)
}

func interactiveDefaultSessionSource(sources []sessionSourceEntry) sessionSourceEntry {
	if len(sources) == 0 {
		return sessionSourceEntry{}
	}
	// Prefer a complete v2 Replica because it carries the exact Agent/Replica
	// tuple required by native resume. The source list is deterministic after
	// buildSessionList, so this remains stable between refreshes.
	for _, source := range sources {
		if source.ReplicaID != "" && source.Complete {
			return source
		}
	}
	for _, source := range sources {
		if source.ReplicaID != "" {
			return source
		}
	}
	return sources[0]
}

func interactiveLegacyActionID(selection interactiveSessionSelection) string {
	if selection.source.legacyID != "" {
		return selection.source.legacyID
	}
	if selection.logicalID != "" {
		return selection.logicalID
	}
	return selection.source.NativeID
}

func runInteractiveResumeSelection(input io.Reader, output, prompt io.Writer, selection interactiveSessionSelection) error {
	source := selection.source
	if source.ReplicaID != "" {
		args := []string{sessionActionResume, selection.logicalID}
		if source.Agent != "" && source.Agent != "unknown" {
			args = append(args, "--agent", source.Agent)
		}
		args = append(args, "--replica", source.ReplicaID)
		return runSessionWithStreams(args, input, output, prompt)
	}
	legacyID := interactiveLegacyActionID(selection)
	if legacyID == "" {
		return errors.New("resume: selected Session has no resumable source")
	}
	return runResumeWithStreams([]string{legacyID}, input, output, prompt)
}

func chooseInteractiveAgent(input io.Reader, output io.Writer, heading string) (string, error) {
	items := []sessionPickerItem{
		{id: "codex", title: "Codex", detail: "Create or inspect a native Codex session"},
		{id: "claude-code", title: "Claude Code", detail: "Create or inspect a native Claude Code session"},
	}
	return runInteractivePicker(input, output, items, interactivePickerOptions{
		errorPrefix:  "session switch",
		heading:      heading,
		help:         "Type to search  |  Up/Down move  |  Enter select  |  Esc back",
		itemNoun:     "agent",
		emptyMessage: "No matching Agents. Keep typing or press Esc.",
		cancelError:  errInteractiveMenuCancelled,
	})
}

func sessionPickerItemsFromReport(report sessionListReport) []sessionPickerItem {
	return sessionPickerItemsFromReportFiltered(report, nil)
}

func sessionPickerItemsFromReportFiltered(report sessionListReport, include func(sessionListEntry) bool) []sessionPickerItem {
	items := make([]sessionPickerItem, 0, len(report.Sessions))
	for _, entry := range report.Sessions {
		if include != nil && !include(entry) {
			continue
		}
		if strings.TrimSpace(entry.SessionID) == "" {
			continue
		}
		agents := make([]string, 0, len(entry.Sources))
		var records uint64
		for _, source := range entry.Sources {
			agents = appendUnique(agents, source.Agent)
			if source.RecordCount > records {
				records = source.RecordCount
			}
		}
		sort.Strings(agents)
		if entry.RecordCount > records {
			records = entry.RecordCount
		}
		updated := entry.UpdatedAt
		if updated.IsZero() {
			updated = entry.CreatedAt
		}
		items = append(items, sessionPickerItem{
			id:        entry.SessionID,
			title:     pickerItemTitle(entry.Title),
			agent:     strings.Join(agents, ","),
			updatedAt: updated,
			records:   records,
		})
	}
	sortSessionPickerItems(items)
	return items
}

func runInteractiveProjectMenu(input io.Reader, output, prompt io.Writer) error {
	return runInteractiveMenu(input, output, prompt, "Projects and policies", []interactiveMenuItem{
		{id: "list", title: "List bound projects", detail: "Review local bindings and sync modes", action: func() error {
			return runProjectWithStreams([]string{projectActionList}, input, output, prompt)
		}},
		{id: "discover", title: "Discover remote projects", detail: "Read project metadata from the backend", action: func() error {
			return runProjectWithStreams([]string{projectActionDiscover}, input, output, prompt)
		}},
		{id: "bind", title: "Bind current project", detail: "Add this workspace to the sync domain", action: func() error {
			return runProjectWithStreams([]string{projectActionBind}, input, output, prompt)
		}},
		{id: "unbind", title: "Unbind current project", detail: "Stop syncing this workspace", action: func() error {
			return runProjectWithStreams([]string{projectActionUnbind, "--path", "."}, input, output, prompt)
		}},
		{id: "normal", title: "Use normal sync", detail: "Pull and push sessions for this project", action: func() error {
			return runProjectWithStreams([]string{projectActionMode, projectModeNormal}, input, output, prompt)
		}},
		{id: "push-only", title: "Use push-only sync", detail: "Publish local sessions; do not restore", action: func() error {
			return runProjectWithStreams([]string{projectActionMode, projectModePushOnly}, input, output, prompt)
		}},
		{id: "excluded", title: "Exclude current project", detail: "Keep this workspace outside sync", action: func() error {
			return runProjectWithStreams([]string{projectActionMode, projectModeExcluded}, input, output, prompt)
		}},
		{id: "back", title: "Back", detail: "Return to the main menu"},
	})
}

func runInteractiveDeviceMenu(input io.Reader, output, prompt io.Writer) error {
	return runInteractiveMenu(input, output, prompt, "Devices", []interactiveMenuItem{
		{id: "status", title: "View local device status", detail: "Identity and operating mode", action: func() error {
			return runDeviceWithStreams([]string{deviceActionStatus}, input, output, prompt)
		}},
		{id: "list", title: "Browse connected devices", detail: "Last activity and device identities", action: func() error {
			return runDeviceWithStreams([]string{deviceActionList}, input, output, prompt)
		}},
		{id: "rename", title: "Rename this device", detail: "Change its display name", action: func() error {
			name, err := readInteractiveLine(input, output, "New device name: ")
			if err != nil {
				return err
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return errors.New("device: name cannot be empty")
			}
			return runDeviceWithStreams([]string{deviceActionRename, name}, input, output, prompt)
		}},
		{id: "normal", title: "Set normal device mode", detail: "Allow regular pull and push", action: func() error {
			return runDeviceWithStreams([]string{deviceActionMode, string(config.DeviceModeNormal)}, input, output, prompt)
		}},
		{id: "push-only", title: "Set push-only device mode", detail: "Publish without restoring", action: func() error {
			return runDeviceWithStreams([]string{deviceActionMode, string(config.DeviceModePushOnly)}, input, output, prompt)
		}},
		{id: "disabled", title: "Disable device sync", detail: "Skip future synchronization", action: func() error {
			return runDeviceWithStreams([]string{deviceActionMode, string(config.DeviceModeDisabled)}, input, output, prompt)
		}},
		{id: "rotate", title: "Rotate device key", detail: "Re-authorize this device", action: func() error {
			return runDeviceWithStreams([]string{deviceActionRotateKey}, input, output, prompt)
		}},
		{id: "invite", title: "Create device invitation", detail: "Authorize another device", action: func() error {
			return runDeviceWithStreams([]string{deviceActionInvite}, input, output, prompt)
		}},
		{id: "remove", title: "Remove a connected device", detail: "Revoke future access immediately", action: func() error {
			selected, err := chooseInteractiveDevice(input, output, prompt)
			if err != nil {
				return err
			}
			return runDeviceWithStreams([]string{deviceActionRemove, selected}, input, output, prompt)
		}},
		{id: "back", title: "Back", detail: "Return to the main menu"},
	})
}

func chooseInteractiveDevice(input io.Reader, output, prompt io.Writer) (string, error) {
	configDir, err := config.Dir()
	if err != nil {
		return "", err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), deviceManagementTimeout)
	defer cancel()
	report, err := collectDeviceList(ctx, c, configDir, input, prompt)
	if err != nil {
		return "", err
	}
	items := make([]sessionPickerItem, 0, len(report.Devices))
	for _, device := range report.Devices {
		if strings.TrimSpace(device.ID) == "" {
			continue
		}
		if device.Local {
			continue
		}
		title := safeListText(device.Name)
		if title == "" || title == "unknown" {
			title = device.ID
		}
		detail := safeListText(device.System)
		if device.Local {
			if detail != "" {
				detail += " | "
			}
			detail += "this device"
		}
		item := sessionPickerItem{id: device.ID, title: title, detail: detail}
		if device.LastActiveAt != nil {
			item.updatedAt = *device.LastActiveAt
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return "", errors.New("device: no connected devices are available")
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].updatedAt.Equal(items[j].updatedAt) {
			return strings.ToLower(items[i].title) < strings.ToLower(items[j].title)
		}
		return items[i].updatedAt.After(items[j].updatedAt)
	})
	return runInteractivePicker(input, output, items, interactivePickerOptions{
		errorPrefix:  "device",
		heading:      "Remove a connected device",
		help:         "Type to search  |  Up/Down move  |  Enter select  |  Esc back",
		itemNoun:     "device",
		emptyMessage: "No matching devices. Keep typing or press Esc.",
		cancelError:  errInteractiveMenuCancelled,
	})
}

func runInteractiveSettingsMenu(input io.Reader, output, prompt io.Writer) error {
	return runInteractiveMenu(input, output, prompt, "Settings and security", []interactiveMenuItem{
		{id: "configure", title: "Configure CtxHop", detail: "Storage backend, domain and device", action: func() error {
			return runInitWithIO(nil, input, output, os.Args[0])
		}},
		{id: "passphrase", title: "Encryption password", detail: "Change or recover the local keyfile", submenu: func() error {
			return runInteractivePassphraseMenu(input, output, prompt)
		}},
		{id: "hooks", title: "Agent hooks", detail: "Install or refresh automatic session pushes", submenu: func() error {
			return runInteractiveHookMenu(input, output, prompt)
		}},
		{id: "back", title: "Back", detail: "Return to the main menu"},
	})
}

func runInteractivePassphraseMenu(input io.Reader, output, prompt io.Writer) error {
	return runInteractiveMenu(input, output, prompt, "Encryption password", []interactiveMenuItem{
		{id: "change", title: "Change encryption password", detail: "Re-encrypt the shared keyfile", action: func() error {
			return runPassphraseWithStreams([]string{passphraseActionChange}, input, output, prompt)
		}},
		{id: "reset", title: "Reset with Recovery Key", detail: "Recover access without changing remote records", action: func() error {
			return runPassphraseWithStreams([]string{passphraseActionReset}, input, output, prompt)
		}},
		{id: "back", title: "Back", detail: "Return to the previous menu"},
	})
}

func runInteractiveHookMenu(input io.Reader, output, prompt io.Writer) error {
	return runInteractiveMenu(input, output, prompt, "Agent hooks", []interactiveMenuItem{
		{id: "all", title: "Install or refresh all hooks", detail: "Enable automatic session pushes", action: func() error {
			return runHookWithIO([]string{hookActionInstall, "--agent", "all"}, input, output, os.Args[0])
		}},
		{id: "codex", title: "Install Codex hook", detail: "Configure the Codex lifecycle hook", action: func() error {
			return runHookWithIO([]string{hookActionInstall, "--agent", "codex"}, input, output, os.Args[0])
		}},
		{id: "claude-code", title: "Install Claude Code hook", detail: "Configure the Claude Code lifecycle hook", action: func() error {
			return runHookWithIO([]string{hookActionInstall, "--agent", "claude-code"}, input, output, os.Args[0])
		}},
		{id: "back", title: "Back", detail: "Return to the previous menu"},
	})
}

func runInteractiveDiagnosticsMenu(input io.Reader, output, prompt io.Writer) error {
	return runInteractiveMenu(input, output, prompt, "Diagnostics", []interactiveMenuItem{
		{id: "status", title: "Show sync status", detail: "Local configuration and current project", action: func() error {
			return runStatusWithStreams(nil, input, output, prompt)
		}},
		{id: "remote-check", title: "Check remote metadata", detail: "Read-only check for updates", action: func() error {
			return runPullWithIO(nil, input, output, prompt)
		}},
		{id: "stats", title: "View restore statistics", detail: "Cross-device restore history", action: func() error {
			return runStatsWithIO(nil, output)
		}},
		{id: "doctor", title: "Run doctor", detail: "Inspect agents, backend and recent errors", action: func() error {
			return runDoctorWithIO(nil, output)
		}},
		{id: "watch-once", title: "Run one watch cycle", detail: "Detect and push current changes once", action: func() error {
			return runWatchWithIO([]string{"--once"}, output)
		}},
		{id: "back", title: "Back", detail: "Return to the main menu"},
	})
}

func runInteractiveRemoteMenu(input io.Reader, output, prompt io.Writer) error {
	return runInteractiveMenu(input, output, prompt, "Remote data", []interactiveMenuItem{
		{id: "delete-session", title: "Delete a remote session", detail: "Select a session, then remove it", action: func() error {
			selected, err := chooseInteractiveLegacySessionSelection(input, output, prompt, "Delete a remote session")
			if err != nil {
				return err
			}
			return runRemoteWithStreams([]string{remoteActionDeleteSession, interactiveLegacyActionID(selected)}, input, output, prompt)
		}},
		{id: "delete-project", title: "Delete the current remote project", detail: "Remove all remote sessions in this project", action: func() error {
			return runRemoteWithStreams([]string{remoteActionDeleteProject}, input, output, prompt)
		}},
		{id: "delete-all", title: "Delete all remote data", detail: "Remove every object in the sync domain", action: func() error {
			return runRemoteWithStreams([]string{remoteActionDeleteAll}, input, output, prompt)
		}},
		{id: "back", title: "Back", detail: "Return to the main menu"},
	})
}

func runInteractiveAdvancedMenu(input io.Reader, output, prompt io.Writer) error {
	return runInteractiveMenu(input, output, prompt, "Advanced tools", []interactiveMenuItem{
		{id: "install", title: "Install command", detail: "Place CtxHop in your user command directory", action: func() error {
			return runInstall(nil)
		}},
		{id: "update", title: "Update CtxHop", detail: "Install the latest release", action: func() error {
			return runUpdate(nil)
		}},
		{id: "uninstall", title: "Remove local installation", detail: "Delete local files and hooks; remote data stays", action: func() error {
			confirmed, err := confirmInteractive(input, prompt, "Remove the local CtxHop command, hooks and configuration? Type 'yes' to continue: ")
			if err != nil {
				return err
			}
			if !confirmed {
				return errInteractiveMenuCancelled
			}
			return runUninstall(nil)
		}},
		{id: "workspace-preview", title: "Preview workspace restore", detail: "Inspect files and Git state before writing", action: func() error {
			selected, err := chooseInteractiveLegacySessionSelection(input, output, prompt, "Preview workspace restore")
			if err != nil {
				return err
			}
			return runWorkspaceWithStreams([]string{"preview", interactiveLegacyActionID(selected)}, input, output, prompt)
		}},
		{id: "workspace-apply", title: "Apply workspace restore", detail: "Write the selected workspace after checks", action: func() error {
			selected, err := chooseInteractiveLegacySessionSelection(input, output, prompt, "Apply workspace restore")
			if err != nil {
				return err
			}
			return runWorkspaceWithStreams([]string{"apply", interactiveLegacyActionID(selected)}, input, output, prompt)
		}},
		{id: "cross-agent-preview", title: "Preview Agent switch", detail: "Choose a session and target Agent first", action: func() error {
			selected, err := chooseInteractiveNativeSessionSelection(input, output, prompt, "Preview Agent switch")
			if err != nil {
				return err
			}
			target, err := chooseInteractiveAgent(input, output, "Choose target Agent")
			if err != nil {
				return err
			}
			return runSessionWithStreams([]string{sessionActionSwitch, "--preview", "--to", target, selected.logicalID}, input, output, prompt)
		}},
		{id: "cross-agent-apply", title: "Switch Agent", detail: "Adapt context into a new native Agent session", action: func() error {
			selected, err := chooseInteractiveNativeSessionSelection(input, output, prompt, "Switch Agent")
			if err != nil {
				return err
			}
			target, err := chooseInteractiveAgent(input, output, "Choose target Agent")
			if err != nil {
				return err
			}
			return runSessionWithStreams([]string{sessionActionSwitch, "--to", target, selected.logicalID}, input, output, prompt)
		}},
		{id: "migration-preview", title: "Preview Session migration", detail: "Inspect legacy-to-Hub mapping read-only", action: func() error {
			return runSessionWithStreams([]string{sessionActionMigrate, "--preview"}, input, output, prompt)
		}},
		{id: "migration-publish", title: "Publish a legacy session", detail: "Create its native Session Hub replica", action: func() error {
			legacyID, err := readInteractiveLine(input, prompt, "Legacy session ID: ")
			if err != nil {
				return err
			}
			legacyID = strings.TrimSpace(legacyID)
			if legacyID == "" {
				return errors.New("session migrate: legacy session ID cannot be empty")
			}
			return runSessionWithStreams([]string{sessionActionMigrate, "--publish-v2", legacyID}, input, output, prompt)
		}},
		{id: "migration-rollback", title: "Roll back a legacy mapping", detail: "Prefer the compatibility reader on this device", action: func() error {
			legacyID, err := readInteractiveLine(input, prompt, "Legacy session ID: ")
			if err != nil {
				return err
			}
			legacyID = strings.TrimSpace(legacyID)
			if legacyID == "" {
				return errors.New("session migrate: legacy session ID cannot be empty")
			}
			return runSessionWithStreams([]string{sessionActionMigrate, "--rollback", legacyID}, input, output, prompt)
		}},
		{id: "history-prune", title: "Prune session history", detail: "Keep a chosen number of newest versions", action: func() error {
			selected, err := chooseInteractiveLegacySessionSelection(input, output, prompt, "Prune session history")
			if err != nil {
				return err
			}
			keepText, err := readInteractiveLine(input, prompt, "Keep how many newest versions? ")
			if err != nil {
				return err
			}
			keep, err := strconv.Atoi(strings.TrimSpace(keepText))
			if err != nil || keep < 0 {
				return errors.New("history prune: enter a non-negative number of versions")
			}
			return runHistoryWithStreams([]string{historyMaintenancePrune, "--keep", strconv.Itoa(keep), interactiveLegacyActionID(selected)}, input, output, prompt)
		}},
		{id: "discover", title: "Discover native sessions", detail: "Read local Agent session indexes", action: func() error {
			return runSessionWithStreams([]string{sessionActionDiscover}, input, output, prompt)
		}},
		{id: "watch", title: "Watch continuously", detail: "Keep pushing changes until interrupted", action: func() error {
			return runWatch(nil)
		}},
		{id: "back", title: "Back", detail: "Return to the main menu"},
	})
}

func confirmInteractive(input io.Reader, prompt io.Writer, question string) (bool, error) {
	answer, err := readInteractiveLine(input, prompt, question)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(answer), "yes"), nil
}

func readInteractiveLine(input io.Reader, output io.Writer, label string) (string, error) {
	if input == nil {
		return "", errors.New("ctxhop: input is required")
	}
	if output == nil {
		return "", errors.New("ctxhop: output is required")
	}
	if _, err := fmt.Fprint(output, label); err != nil {
		return "", err
	}
	var line []byte
	var one [1]byte
	for {
		read, err := input.Read(one[:])
		if read > 0 {
			if one[0] == '\n' {
				break
			}
			line = append(line, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}
