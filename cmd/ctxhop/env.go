package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/CCCCY-ci/ctxhop/internal/environment"
)

type environmentPreviewReport struct {
	Scope        string                         `json:"scope"`
	Session      string                         `json:"session"`
	Agent        string                         `json:"agent,omitempty"`
	NativeID     string                         `json:"nativeId,omitempty"`
	Dependencies []environment.Reference        `json:"dependencies,omitempty"`
	Requirements []environmentRequirementChange `json:"requirements,omitempty"`
	HookState    string                         `json:"hookState,omitempty"`
	Components   []environment.Component        `json:"components,omitempty"`
	Changes      []environmentComponentChange   `json:"changes,omitempty"`
	Status       string                         `json:"status"`
	Notes        []string                       `json:"notes"`
}

func findEnvironmentSession(sessions []listSession, requested string) *listSession {
	for i := range sessions {
		if sessions[i].RemoteID == requested {
			return &sessions[i]
		}
	}
	for i := range sessions {
		if sessions[i].NativeID == requested {
			return &sessions[i]
		}
	}
	return nil
}

func writeEnvironmentPreviewJSON(w io.Writer, report environmentPreviewReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeEnvironmentPreviewText(w io.Writer, report environmentPreviewReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "session: %s\n", safeListText(report.Session)); err != nil {
		return err
	}
	if report.NativeID != "" {
		if _, err := fmt.Fprintf(w, "native session: %s\n", safeListText(report.NativeID)); err != nil {
			return err
		}
	}
	if report.Agent != "" {
		if _, err := fmt.Fprintf(w, "agent: %s\n", safeListText(report.Agent)); err != nil {
			return err
		}
	}
	if report.HookState != "" {
		if _, err := fmt.Fprintf(w, "local hook: %s\n", safeListText(report.HookState)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "status: %s\n", report.Status); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "dependencies: %d\n", len(report.Dependencies)); err != nil {
		return err
	}
	for _, dependency := range report.Dependencies {
		version := ""
		if dependency.Version != "" {
			version = " version=" + safeListText(dependency.Version)
		}
		if _, err := fmt.Fprintf(w, "- kind=%s name=%s portability=%s%s\n",
			safeListText(dependency.Kind),
			safeListText(dependency.Name),
			safeListText(dependency.Portability),
			version,
		); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "tool requirements: %d\n", len(report.Requirements)); err != nil {
		return err
	}
	for _, requirement := range report.Requirements {
		line := fmt.Sprintf("- requirement kind=%s name=%s state=%s",
			safeListText(requirement.Dependency.Kind),
			safeListText(requirement.Dependency.Name),
			safeListText(requirement.State),
		)
		if requirement.Dependency.Version != "" {
			line += " observed-version=" + safeListText(requirement.Dependency.Version)
		}
		if requirement.LocalVersion != "" {
			line += " local-version=" + safeListText(requirement.LocalVersion)
		}
		if requirement.LocalVersionSource != "" {
			line += " local-version-source=" + safeListText(requirement.LocalVersionSource)
		}
		if requirement.Reason != "" {
			line += " reason=" + safeListText(requirement.Reason)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "components: %d\n", len(report.Components)); err != nil {
		return err
	}
	for _, component := range report.Components {
		if _, err := fmt.Fprintf(w, "- component kind=%s name=%s scope=%s size=%d fingerprint=%s\n",
			safeListText(component.Kind),
			safeListText(component.Name),
			safeListText(component.Scope),
			component.Size,
			safeListText(component.Fingerprint),
		); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "changes: %d\n", len(report.Changes)); err != nil {
		return err
	}
	for _, change := range report.Changes {
		line := fmt.Sprintf("- change kind=%s name=%s scope=%s state=%s",
			safeListText(change.Component.Kind),
			safeListText(change.Component.Name),
			safeListText(change.Component.Scope),
			safeListText(change.State),
		)
		if change.Path != "" {
			line += " path=" + safeListText(change.Path)
		}
		if change.Backup != "" {
			line += " backup=" + safeListText(change.Backup)
		}
		if change.Reason != "" {
			line += " reason=" + safeListText(change.Reason)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	for _, note := range report.Notes {
		if _, err := fmt.Fprintf(w, "note: %s\n", safeListText(note)); err != nil {
			return err
		}
	}
	return nil
}
