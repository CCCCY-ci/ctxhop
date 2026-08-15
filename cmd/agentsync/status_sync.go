package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/project"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

type statusSync struct {
	Checked  bool                `json:"checked"`
	Mode     string              `json:"mode,omitempty"`
	Backend  string              `json:"backend,omitempty"`
	Sessions statusSessionCounts `json:"sessions"`
	Queue    statusQueueCounts   `json:"queue"`
}

type statusSessionCounts struct {
	Local          int `json:"local"`
	Remote         int `json:"remote"`
	Synced         int `json:"synced"`
	LocalOnly      int `json:"localOnly"`
	RemoteOnly     int `json:"remoteOnly"`
	ForeignUpdates int `json:"foreignUpdates"`
	Attention      int `json:"attention"`
}

type statusQueueCounts struct {
	Pending        int `json:"pending"`
	Due            int `json:"due"`
	RetryScheduled int `json:"retryScheduled"`
	Blocked        int `json:"blocked"`
}

const statusRemoteTimeout = 15 * time.Second

// collectRemoteStatus checks project metadata without reading session shards
// or writing Agent data. It is opt-in because unlocking the private identity
// requires a passphrase and a normal status check must remain non-interactive.
func collectRemoteStatus(ctx context.Context, c *config.Config, configDir, projectDir string, input io.Reader, prompt io.Writer) (statusSync, error) {
	if ctx == nil {
		return statusSync{}, errors.New("status: context is required")
	}
	if c == nil {
		return statusSync{}, errors.New("status: configuration is unavailable")
	}
	if input == nil {
		return statusSync{}, errors.New("status: input is required for --remote")
	}
	if prompt == nil {
		return statusSync{}, errors.New("status: prompt output is required for --remote")
	}
	if err := ctx.Err(); err != nil {
		return statusSync{}, fmt.Errorf("status: %w", err)
	}

	current, err := project.Identify(ctx, projectDir)
	if err != nil {
		return statusSync{}, fmt.Errorf("status: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return statusSync{}, fmt.Errorf("status: %s", reason)
	}

	switch projectPullMode(c, current.Identity.Value) {
	case projectModeExcluded:
		return statusSync{Mode: projectModeExcluded}, nil
	case projectModePushOnly:
		return statusSync{Mode: projectModePushOnly}, nil
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return statusSync{}, fmt.Errorf("status: local device identity is invalid: %w", err)
	}
	if len(c.IdentityPublic) == 0 {
		return statusSync{}, errors.New("status: encryption identity is not configured; run 'agentsync init'")
	}

	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return statusSync{}, fmt.Errorf("status: load local sync material: %w", err)
	}
	projectID, err := crypto.ProjectID(secrets.IdentifierKey, current.Identity.Value)
	if err != nil {
		return statusSync{}, fmt.Errorf("status: derive project identity: %w", err)
	}
	store, err := buildConfiguredRemote(c, configDir)
	if err != nil {
		return statusSync{}, fmt.Errorf("status: configure backend: %s", safeBackendSetupError(err))
	}
	keyfile, err := syncer.FetchKeyfile(ctx, store)
	if err != nil {
		return statusSync{}, fmt.Errorf("status: read remote keyfile: %w", err)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		return statusSync{}, fmt.Errorf("status: validate remote identity: %w", err)
	}
	if !bytes.Equal(public.Bytes(), c.IdentityPublic) {
		return statusSync{}, errors.New("status: remote encryption identity does not match this configuration")
	}

	passphrase, err := readListPassphrase(input, prompt)
	if err != nil {
		return statusSync{}, err
	}
	dataKey, err := keyfile.UnlockWithPassphrase(passphrase)
	if err != nil {
		return statusSync{}, fmt.Errorf("status: unlock remote keyfile: %w", err)
	}
	defer dataKey.Close()
	identity, err := dataKey.IdentityPrivate()
	if err != nil {
		return statusSync{}, fmt.Errorf("status: open remote identity: %w", err)
	}

	remoteSessions, err := syncer.FetchProjectMetadata(ctx, store, projectID, identity)
	if errors.Is(err, syncer.ErrNoRemoteMetadata) {
		remoteSessions = nil
	} else if err != nil {
		return statusSync{}, fmt.Errorf("status: read encrypted session metadata: %w", err)
	}
	localSessions, err := discoverListSessions(current.Root)
	if err != nil {
		return statusSync{}, err
	}

	queue, err := syncer.NewQueueStore(configDir)
	if err != nil {
		return statusSync{}, fmt.Errorf("status: prepare pending queue: %w", err)
	}
	queueSnapshot, err := queue.Load(ctx)
	if err != nil {
		return statusSync{}, fmt.Errorf("status: read pending queue: %w", err)
	}

	counts, err := calculateStatusSessions(ctx, c.Device.ID, projectID, secrets.IdentifierKey, configDir, localSessions, remoteSessions)
	if err != nil {
		return statusSync{}, err
	}
	return statusSync{
		Checked:  true,
		Mode:     "remote",
		Backend:  store.Name(),
		Sessions: counts,
		Queue:    countStatusQueue(queueSnapshot, projectID, c.Device.ID, time.Now().UTC()),
	}, nil
}

// calculateStatusSessions compares authenticated metadata with the local
// device cursor. It never reads encrypted shard bodies.
func calculateStatusSessions(ctx context.Context, localDeviceID, projectID string, identifierKey []byte, stateRoot string, local []adapter.SessionRef, remoteSessions []syncer.ProjectMetadataRef) (statusSessionCounts, error) {
	counts := statusSessionCounts{Local: len(local), Remote: len(remoteSessions)}
	localByID := make(map[string]struct{}, len(local))
	for _, ref := range local {
		sessionID, err := crypto.SessionID(identifierKey, projectID, ref.NativeID)
		if err != nil {
			continue
		}
		localByID[sessionID] = struct{}{}
	}

	seen := make(map[string]struct{}, len(remoteSessions))
	for _, remoteSession := range remoteSessions {
		seen[remoteSession.SessionID] = struct{}{}
		_, isLocal := localByID[remoteSession.SessionID]
		if !isLocal {
			counts.RemoteOnly++
			continue
		}

		layout, err := syncer.NewObjectLayout(projectID, remoteSession.SessionID, localDeviceID)
		if err != nil {
			counts.Attention++
			continue
		}
		cursorStore, err := syncer.NewCursorStore(stateRoot, layout)
		if err != nil {
			counts.Attention++
			continue
		}
		cursor, err := cursorStore.Load(ctx)
		if errors.Is(err, syncer.ErrNoPushCursor) {
			cursor = syncer.NewPushCursor()
		} else if err != nil {
			counts.Attention++
			continue
		}
		pullTipStore, err := syncer.NewPullTipStore(stateRoot, layout)
		if err != nil {
			counts.Attention++
			continue
		}
		observed, err := syncflow.LoadObservedTips(ctx, pullTipStore)
		if err != nil {
			counts.Attention++
			continue
		}
		plan, err := syncflow.PlanPull(remoteSession.Devices, syncflow.PullOptions{
			LocalDeviceID: localDeviceID,
			LocalCursor:   cursor,
			Observed:      observed,
		})
		if err != nil {
			counts.Attention++
			continue
		}
		if plan.HasForeignChanges() {
			counts.ForeignUpdates++
		} else {
			counts.Synced++
		}
	}

	for sessionID := range localByID {
		if _, exists := seen[sessionID]; !exists {
			counts.LocalOnly++
		}
	}
	return counts, nil
}

func countStatusQueue(snapshot syncer.QueueSnapshot, projectID, deviceID string, now time.Time) statusQueueCounts {
	var counts statusQueueCounts
	for _, item := range snapshot.Items {
		if item.Key.ProjectID != projectID || item.Key.DeviceID != deviceID {
			continue
		}
		switch item.State {
		case syncer.QueuePending:
			counts.Pending++
			if item.NextAttemptAt.IsZero() || !item.NextAttemptAt.After(now) {
				counts.Due++
			} else {
				counts.RetryScheduled++
			}
		case syncer.QueueBlocked:
			counts.Blocked++
		}
	}
	return counts
}

func writeStatusSyncText(w io.Writer, sync statusSync) error {
	if _, err := fmt.Fprintln(w, "sync:"); err != nil {
		return err
	}
	if sync.Mode == "excluded" || sync.Mode == "push-only" {
		_, err := fmt.Fprintf(w, "  mode: %s\n", sync.Mode)
		return err
	}
	if _, err := fmt.Fprintf(w, "  backend: %s\n", sync.Backend); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  sessions: local=%d remote=%d synced=%d local-only=%d remote-only=%d foreign-updates=%d attention=%d\n",
		sync.Sessions.Local,
		sync.Sessions.Remote,
		sync.Sessions.Synced,
		sync.Sessions.LocalOnly,
		sync.Sessions.RemoteOnly,
		sync.Sessions.ForeignUpdates,
		sync.Sessions.Attention); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "  queue: pending=%d due=%d retry-scheduled=%d blocked=%d\n",
		sync.Queue.Pending,
		sync.Queue.Due,
		sync.Queue.RetryScheduled,
		sync.Queue.Blocked)
	return err
}
