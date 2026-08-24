// Command mvp runs the repository's local, synthetic MVP acceptance matrix.
//
// It uses the production syncer, remote and restore planner with generated
// canonical records. It never contacts a real backend and never prints session
// contents, paths or credentials. Real Windows/macOS/Linux and provider
// matrix runs remain separate environment acceptance work.
package main

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

const (
	mvpProject = "project"
	mvpSession = "session"
	mvpDeviceA = "devicea"
	mvpDeviceB = "deviceb"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mvp: FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	root, err := os.MkdirTemp("", "ctxhop-mvp-")
	if err != nil {
		return fmt.Errorf("create temporary acceptance root: %w", err)
	}
	defer os.RemoveAll(root)

	store, err := remote.NewDir(filepath.Join(root, "remote"))
	if err != nil {
		return err
	}

	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		return fmt.Errorf("open encryption public key: %w", err)
	}
	identity, err := dataKey.IdentityPrivate()
	if err != nil {
		return fmt.Errorf("open encryption private key: %w", err)
	}

	first := [][]byte{
		[]byte(`{"cwd":"${AS_PROJECT}","message":{"content":"one"}}`),
		[]byte(`{"cwd":"${AS_PROJECT}","message":{"content":"two"}}`),
	}
	second := [][]byte{
		first[0],
		[]byte(`{"cwd":"${AS_PROJECT}","message":{"content":"branch"}}`),
	}

	var cursorA syncer.PushCursor
	if err := step("device A durable push and metadata", func() error {
		var err error
		cursorA, err = publishSession(ctx, store, public, filepath.Join(root, "state-a"), mvpProject, mvpSession, mvpDeviceA, first)
		return err
	}); err != nil {
		return err
	}
	if cursorA.RecordCount != uint64(len(first)) {
		return fmt.Errorf("device A cursor count = %d, want %d", cursorA.RecordCount, len(first))
	}

	if err := step("metadata-only foreign-device check", func() error {
		counted := &countingRemote{Remote: store}
		plan, err := syncflow.FetchPullPlan(ctx, counted, mvpProject, mvpSession, identity, syncflow.PullOptions{
			LocalDeviceID: mvpDeviceB,
			LocalCursor:   syncer.NewPushCursor(),
		})
		if err != nil {
			return err
		}
		if len(plan.Foreign) != 1 || plan.Foreign[0].DeviceID != mvpDeviceA {
			return fmt.Errorf("foreign tips = %v, want device A", plan.Foreign)
		}
		if len(counted.getKeys) == 0 {
			return errors.New("metadata check did not read any authenticated metadata")
		}
		for _, key := range counted.getKeys {
			if !strings.HasSuffix(key, "/meta") && !strings.HasSuffix(key, "/env") {
				return fmt.Errorf("metadata-only check read session shard object %q", key)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if err := step("stale List cannot restore a truncated branch", func() error {
		layout, err := syncer.NewObjectLayout(mvpProject, mvpSession, mvpDeviceA)
		if err != nil {
			return err
		}
		hidden, err := layout.ShardKey(2)
		if err != nil {
			return err
		}
		stale := &filterListRemote{Remote: store, hiddenKey: hidden}
		_, err = syncer.FetchCompleteBranches(ctx, stale, mvpProject, mvpSession, identity)
		if err == nil || !errors.Is(err, syncer.ErrIncompleteRemoteSession) {
			return fmt.Errorf("error = %v, want ErrIncompleteRemoteSession", err)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := step("complete branch restore planning", func() error {
		branches, err := syncer.FetchCompleteBranches(ctx, store, mvpProject, mvpSession, identity)
		if err != nil {
			return err
		}
		if len(branches) != 1 || len(branches[0].Records) != len(first) {
			return fmt.Errorf("complete branches = %d/%d, want 1/%d", len(branches), len(branches[0].Records), len(first))
		}
		plan, err := syncflow.FetchRestorePlan(
			ctx,
			store,
			mvpProject,
			mvpSession,
			identity,
			adapter.PathSpace{ProjectRoot: "/target/project", AgentHome: "/target/agent"},
			adapter.Installation{Compatibility: adapter.CompatFull, CompatibilityReason: "synthetic MVP matrix"},
			syncflow.RestoreOptions{},
		)
		if err != nil {
			return err
		}
		if plan.ResolutionKind != syncer.ResolutionConsistent || len(plan.CanonicalRecords) != len(first) {
			return fmt.Errorf("restore plan = kind %v records %d", plan.ResolutionKind, len(plan.CanonicalRecords))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := step("fork is preserved and requires explicit selection", func() error {
		if _, err := publishSession(ctx, store, public, filepath.Join(root, "state-b"), mvpProject, mvpSession, mvpDeviceB, second); err != nil {
			return err
		}
		space := adapter.PathSpace{ProjectRoot: "/target/project", AgentHome: "/target/agent"}
		installation := adapter.Installation{Compatibility: adapter.CompatFull, CompatibilityReason: "synthetic MVP matrix"}
		_, err := syncflow.FetchRestorePlan(ctx, store, mvpProject, mvpSession, identity, space, installation, syncflow.RestoreOptions{})
		if err == nil || !errors.Is(err, syncflow.ErrForkSelectionRequired) {
			return fmt.Errorf("unselected fork error = %v, want ErrForkSelectionRequired", err)
		}
		selected := 1
		plan, err := syncflow.FetchRestorePlan(ctx, store, mvpProject, mvpSession, identity, space, installation, syncflow.RestoreOptions{
			VersionIndex: &selected,
		})
		if err != nil {
			return err
		}
		if plan.ResolutionKind != syncer.ResolutionFork || len(plan.CanonicalRecords) != len(second) {
			return fmt.Errorf("selected fork plan = kind %v records %d", plan.ResolutionKind, len(plan.CanonicalRecords))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := step("cancelled remote read fails closed", func() error {
		cancelled, stop := context.WithCancel(ctx)
		stop()
		_, err := syncer.FetchCompleteBranches(cancelled, store, mvpProject, mvpSession, identity)
		if err == nil || !errors.Is(err, context.Canceled) {
			return fmt.Errorf("cancelled read error = %v, want context.Canceled", err)
		}
		return nil
	}); err != nil {
		return err
	}

	fmt.Println("mvp: PASS: local acceptance matrix complete")
	return nil
}

func step(name string, run func() error) error {
	if err := run(); err != nil {
		fmt.Printf("mvp: FAIL: %s: %v\n", name, err)
		return fmt.Errorf("%s: %w", name, err)
	}
	fmt.Printf("mvp: PASS: %s\n", name)
	return nil
}

func publishSession(ctx context.Context, store remote.Remote, public *ecdh.PublicKey, stateRoot, projectID, sessionID, deviceID string, records [][]byte) (syncer.PushCursor, error) {
	layout, err := syncer.NewObjectLayout(projectID, sessionID, deviceID)
	if err != nil {
		return syncer.PushCursor{}, err
	}
	state, err := syncer.NewCursorStore(stateRoot, layout)
	if err != nil {
		return syncer.PushCursor{}, err
	}
	cursor := syncer.NewPushCursor()
	if err := state.Save(ctx, cursor); err != nil {
		return syncer.PushCursor{}, err
	}
	options := syncer.DefaultPlanOptions()
	options.MaxRecords = 1
	executor, err := syncer.NewAppendExecutor(store, public, layout, state, options)
	if err != nil {
		return syncer.PushCursor{}, err
	}
	cursor, err = executor.Execute(ctx, cursor, records)
	if err != nil {
		return syncer.PushCursor{}, err
	}
	if err := executor.PublishMetadata(ctx, cursor, []byte(`{"poc":true}`)); err != nil {
		return syncer.PushCursor{}, err
	}
	return cursor, nil
}

type countingRemote struct {
	remote.Remote
	getKeys []string
}

func (r *countingRemote) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	r.getKeys = append(r.getKeys, key)
	return r.Remote.Get(ctx, key)
}

type filterListRemote struct {
	remote.Remote
	hiddenKey string
}

func (r *filterListRemote) List(ctx context.Context, prefix string) ([]remote.ObjectInfo, error) {
	objects, err := r.Remote.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	filtered := objects[:0]
	for _, object := range objects {
		if object.Key != r.hiddenKey {
			filtered = append(filtered, object)
		}
	}
	return filtered, nil
}
