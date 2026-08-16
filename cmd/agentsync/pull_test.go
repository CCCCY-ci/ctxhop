package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestParsePullOptions(t *testing.T) {
	options, err := parsePullOptions([]string{"--check", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.check || !options.json {
		t.Fatalf("options = %+v", options)
	}

	for _, args := range [][]string{
		nil,
		{"--json"},
		{"--check", "unexpected"},
		{"--unknown"},
	} {
		if _, err := parsePullOptions(args); err == nil {
			t.Errorf("parsePullOptions(%v) accepted invalid input", args)
		}
	}
}

func TestPullCheckCommandIsRegistered(t *testing.T) {
	for _, command := range commands {
		if command.name == "pull" {
			if command.run == nil {
				t.Fatal("pull command has no handler")
			}
			return
		}
	}
	t.Fatal("pull command is missing")
}

func TestPullCheckReportOutput(t *testing.T) {
	report := newPullCheckReport(pullCheckSessionCounts{
		Checked:         3,
		ForeignUpdates:  1,
		ForeignBranches: 1,
		Unchanged:       2,
	})
	var textOutput bytes.Buffer
	if err := writePullCheckText(&textOutput, report); err != nil {
		t.Fatal(err)
	}
	want := "scope: project\ncheck: metadata-only\nresult: updates-available\nsessions: checked=3 foreign-updates=1 foreign-branches=1 unchanged=2 attention=0\n"
	if textOutput.String() != want {
		t.Errorf("text output = %q, want %q", textOutput.String(), want)
	}

	var jsonOutput bytes.Buffer
	if err := writePullCheckJSON(&jsonOutput, report); err != nil {
		t.Fatal(err)
	}
	var decoded pullCheckReport
	if err := json.Unmarshal(jsonOutput.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Mode != pullCheckModeMetadataOnly || decoded.Result != pullCheckResultUpdates || decoded.Sessions.Checked != 3 {
		t.Fatalf("JSON report = %+v", decoded)
	}
}

func TestCollectPullCheckStopsDeviceBoundaryBeforeProjectAndInput(t *testing.T) {
	for _, mode := range []config.DeviceMode{config.DeviceModePushOnly, config.DeviceModeDisabled} {
		c := config.New()
		c.Remote.Type = "dir"
		c.Device.Mode = mode
		_, err := collectPullCheck(context.Background(), c, t.TempDir(), filepath.Join(t.TempDir(), "not-a-project"), nil, nil)
		if err == nil || !strings.Contains(err.Error(), string(mode)) {
			t.Fatalf("mode %q error = %v", mode, err)
		}
	}
}

func TestCollectPullCheckReadsMetadataOnlyAndDoesNotSaveObservedTips(t *testing.T) {
	fixture := newPullCheckFixture(t)

	var prompt bytes.Buffer
	report, err := collectPullCheck(context.Background(), fixture.config, fixture.configDir, fixture.projectRoot, strings.NewReader("passphrase\n"), &prompt)
	if err != nil {
		t.Fatal(err)
	}
	if report.Result != pullCheckResultUpdates {
		t.Fatalf("result = %+v", report)
	}
	if report.Sessions.Checked != 1 || report.Sessions.ForeignUpdates != 1 || report.Sessions.ForeignBranches != 1 || report.Sessions.Attention != 0 {
		t.Fatalf("sessions = %+v", report.Sessions)
	}
	if prompt.String() != "Passphrase: " {
		t.Errorf("prompt = %q", prompt.String())
	}
	if _, err := os.Stat(filepath.Join(fixture.configDir, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pull check wrote local state: %v", err)
	}
}

func TestCollectPullCheckSuppressesObservedForeignTip(t *testing.T) {
	fixture := newPullCheckFixture(t)
	layout, err := syncer.NewObjectLayout(fixture.projectID, fixture.sessionID, fixture.config.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	pullTipStore, err := syncer.NewPullTipStore(fixture.configDir, layout)
	if err != nil {
		t.Fatal(err)
	}
	tip, err := syncer.NewPullTip(fixture.foreignDevice, 2, [32]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	if err := pullTipStore.Save(context.Background(), []syncer.PullTip{tip}); err != nil {
		t.Fatal(err)
	}

	report, err := collectPullCheck(context.Background(), fixture.config, fixture.configDir, fixture.projectRoot, strings.NewReader("passphrase\n"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if report.Result != pullCheckResultUpToDate || report.Sessions.ForeignUpdates != 0 || report.Sessions.Unchanged != 1 {
		t.Fatalf("report after observed tip = %+v", report)
	}
}

type pullCheckFixture struct {
	config        *config.Config
	configDir     string
	projectRoot   string
	projectID     string
	sessionID     string
	foreignDevice string
	store         *remote.Dir
}

func newPullCheckFixture(t *testing.T) pullCheckFixture {
	t.Helper()
	projectRoot := t.TempDir()
	runPullCheckGit(t, projectRoot, "init", "-q")
	runPullCheckGit(t, projectRoot, "remote", "add", "origin", "https://github.com/example/project.git")

	remoteRoot := t.TempDir()
	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	keyfile, _, err := crypto.NewKeyfile("passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.PublishKeyfile(context.Background(), store, keyfile); err != nil {
		t.Fatal(err)
	}
	dataKey, err := keyfile.UnlockWithPassphrase("passphrase")
	if err != nil {
		t.Fatal(err)
	}
	identifierKey, err := dataKey.IdentifierKey()
	if err != nil {
		dataKey.Close()
		t.Fatal(err)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		dataKey.Close()
		t.Fatal(err)
	}
	dataKey.Close()

	configDir := t.TempDir()
	c := config.New()
	c.Remote = config.Remote{Type: "dir", Path: remoteRoot}
	c.IdentityPublic = append([]byte(nil), public.Bytes()...)
	if err := config.SaveSecrets(configDir, &config.Secrets{IdentifierKey: identifierKey}); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(configDir); err != nil {
		t.Fatal(err)
	}
	if _, err := config.EnsureDeviceID(configDir, c, identifierKey); err != nil {
		t.Fatal(err)
	}

	projectID, err := crypto.ProjectID(identifierKey, "github.com/example/project")
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := crypto.SessionID(identifierKey, projectID, "foreign-session")
	if err != nil {
		t.Fatal(err)
	}
	foreignDevice := "foreigndevice"
	layout, err := syncer.NewObjectLayout(projectID, sessionID, foreignDevice)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := syncer.NewMetadata(2, [32]byte{2}, []byte("{\"version\":1}"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := layout.MetadataKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := syncer.SealMetadata(public, key, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		t.Fatal(err)
	}
	shardKey, err := layout.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	shardBody := []byte("this body must not be read by pull check")
	if err := store.Put(context.Background(), shardKey, bytes.NewReader(shardBody), int64(len(shardBody))); err != nil {
		t.Fatal(err)
	}

	return pullCheckFixture{
		config:        c,
		configDir:     configDir,
		projectRoot:   projectRoot,
		projectID:     projectID,
		sessionID:     sessionID,
		foreignDevice: foreignDevice,
		store:         store,
	}
}

func runPullCheckGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, output)
	}
}
