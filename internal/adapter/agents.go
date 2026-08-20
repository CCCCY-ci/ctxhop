package adapter

import (
	"context"
	"errors"
	"fmt"
)

// DefaultLayouts returns the built-in agent layouts in a stable order. Claude
// remains first for backward-compatible discovery; project sessions are still
// collected from every installed layout, so Codex and Claude can coexist.
func DefaultLayouts() ([]SessionLayout, error) {
	claudeHome, err := DefaultHome()
	if err != nil {
		return nil, fmt.Errorf("locate Claude Code: %w", err)
	}
	codexHome, err := DefaultCodexHome()
	if err != nil {
		return nil, fmt.Errorf("locate Codex: %w", err)
	}
	return []SessionLayout{
		Layout{Home: claudeHome},
		CodexLayout{Home: codexHome},
	}, nil
}

// DiscoverInstalled returns every installed built-in agent and its sessions
// for projectRoot. An absent agent is normal and is skipped.
func DiscoverInstalled(ctx context.Context, projectRoot string) ([]AgentSessions, error) {
	if ctx == nil {
		return nil, errors.New("adapter: context is required")
	}
	layouts, err := DefaultLayouts()
	if err != nil {
		return nil, err
	}
	var installed []AgentSessions
	for _, layout := range layouts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		installation, err := layout.Detect(ctx)
		if errors.Is(err, ErrNotInstalled) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", layout.Name(), err)
		}
		var sessions []SessionRef
		if projectRoot != "" {
			sessions, err = layout.DiscoverSessions(projectRoot)
			if err != nil {
				return nil, fmt.Errorf("discover %s sessions: %w", layout.Name(), err)
			}
		}
		for i := range sessions {
			if sessions[i].Agent == "" {
				sessions[i].Agent = layout.Name()
			}
		}
		installed = append(installed, AgentSessions{
			Layout:       layout,
			Installation: installation,
			Sessions:     sessions,
		})
	}
	return installed, nil
}

// FindInstalled locates one named built-in agent. It is used by resume when a
// remote summary records the source adapter explicitly.
func FindInstalled(ctx context.Context, name string) (AgentSessions, error) {
	if ctx == nil {
		return AgentSessions{}, errors.New("adapter: context is required")
	}
	layouts, err := DefaultLayouts()
	if err != nil {
		return AgentSessions{}, err
	}
	for _, layout := range layouts {
		if layout.Name() != name {
			continue
		}
		installation, err := layout.Detect(ctx)
		if err != nil {
			return AgentSessions{}, err
		}
		return AgentSessions{Layout: layout, Installation: installation}, nil
	}
	return AgentSessions{}, fmt.Errorf("adapter: unsupported agent %q", name)
}
