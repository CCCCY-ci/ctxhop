package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func collectProjectDiscover(ctx context.Context, c *config.Config, configDir string, input io.Reader, prompt io.Writer) (projectDiscoverReport, error) {
	if c == nil {
		return projectDiscoverReport{}, errors.New("project discover: configuration is unavailable")
	}
	if ctx == nil {
		return projectDiscoverReport{}, errors.New("project discover: context is required")
	}
	if err := ctx.Err(); err != nil {
		return projectDiscoverReport{}, fmt.Errorf("project discover: %w", err)
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return projectDiscoverReport{}, fmt.Errorf("project discover: local device identity is invalid: %w", err)
	}

	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "project discover")
	if err != nil {
		return projectDiscoverReport{}, err
	}
	defer access.close()

	announcements, err := syncer.FetchProjectAnnouncements(ctx, access.Store, access.Identities, access.allowedDevices())
	if err != nil {
		return projectDiscoverReport{}, fmt.Errorf("project discover: read encrypted project index: %w", err)
	}

	latest := make(map[string]syncer.ProjectAnnouncement, len(announcements))
	for _, announcement := range announcements {
		previous, exists := latest[announcement.ProjectID]
		if exists && (previous.IdentityKind != announcement.IdentityKind || previous.Identity != announcement.Identity) {
			return projectDiscoverReport{}, fmt.Errorf("project discover: %w for %q", syncer.ErrConflictingProjectAnnouncement, announcement.ProjectID)
		}
		if !exists || announcement.AnnouncedAt.After(previous.AnnouncedAt) {
			latest[announcement.ProjectID] = announcement
		}
	}

	projects := make([]projectDiscoverEntry, 0, len(latest))
	for _, announcement := range latest {
		projects = append(projects, projectDiscoverEntry{
			ProjectID:    announcement.ProjectID,
			IdentityKind: announcement.IdentityKind,
			Identity:     announcement.Identity,
			AnnouncedAt:  announcement.AnnouncedAt.UTC(),
			Bound:        projectBindingExists(c, announcement.Identity),
		})
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Identity != projects[j].Identity {
			return projects[i].Identity < projects[j].Identity
		}
		return projects[i].ProjectID < projects[j].ProjectID
	})
	return projectDiscoverReport{Scope: "remote", Projects: projects}, nil
}

func projectBindingExists(c *config.Config, identity string) bool {
	if c == nil {
		return false
	}
	for _, binding := range c.Projects.Bindings {
		if binding.Identity == identity {
			return true
		}
	}
	return false
}

func writeProjectDiscoverJSON(w io.Writer, report projectDiscoverReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeProjectDiscoverText(w io.Writer, report projectDiscoverReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "projects: %d\n", len(report.Projects)); err != nil {
		return err
	}
	for _, project := range report.Projects {
		bound := "new"
		if project.Bound {
			bound = "bound"
		}
		if _, err := fmt.Fprintf(w, "- identity=%s kind=%s announced=%s %s\n",
			safeListText(project.Identity),
			safeListText(project.IdentityKind),
			project.AnnouncedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			bound,
		); err != nil {
			return err
		}
	}
	return nil
}
