package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

const (
	queueVersion = 1
	queueFile    = "queue.json"
)

var (
	// ErrInvalidQueue reports malformed or internally inconsistent queue data.
	ErrInvalidQueue = errors.New("syncer: invalid pending queue")

	// ErrUnsupportedQueue reports a queue written by a newer format version.
	ErrUnsupportedQueue = errors.New("syncer: pending queue is newer than this version")

	// ErrQueueItemMissing reports an operation for a task that is not queued.
	ErrQueueItemMissing = errors.New("syncer: pending queue item does not exist")

	// ErrQueueFileMissing reports that the queue file itself disappeared while
	// a queue operation was running. It wraps ErrQueueItemMissing for callers
	// that still need to classify the missing task.
	ErrQueueFileMissing = errors.New("syncer: pending queue file does not exist")

	// ErrQueueItemBlocked reports an attempt to retry a terminally blocked task.
	ErrQueueItemBlocked = errors.New("syncer: pending queue item is blocked")

	// ErrRetryExhausted reports that the attempt counter cannot advance safely.
	ErrRetryExhausted = errors.New("syncer: pending queue retry count is exhausted")
)

// QueueKey identifies a pending task without retaining any local path or
// session content.
type QueueKey struct {
	ProjectID string
	SessionID string
	DeviceID  string
}

// NewQueueKey validates the opaque identifiers used by a pending task.
func NewQueueKey(projectID, sessionID, deviceID string) (QueueKey, error) {
	key := QueueKey{
		ProjectID: projectID,
		SessionID: sessionID,
		DeviceID:  deviceID,
	}
	if err := key.Validate(); err != nil {
		return QueueKey{}, err
	}
	return key, nil
}

// Validate checks the identifier shape accepted by the queue wire format.
func (k QueueKey) Validate() error {
	for name, value := range map[string]string{
		"project ID": k.ProjectID,
		"session ID": k.SessionID,
		"device ID":  k.DeviceID,
	} {
		if err := validateQueueIdentifier(name, value); err != nil {
			return err
		}
	}
	return nil
}

func validateQueueIdentifier(name, value string) error {
	if value == "" {
		return fmt.Errorf("syncer: %s is required", name)
	}
	if len(value) > 128 {
		return fmt.Errorf("syncer: %s is too long", name)
	}
	if err := validateIdentifier(value); err != nil {
		return fmt.Errorf("syncer: %s is invalid: %w", name, err)
	}
	return nil
}

func (k QueueKey) less(other QueueKey) bool {
	if k.ProjectID != other.ProjectID {
		return k.ProjectID < other.ProjectID
	}
	if k.SessionID != other.SessionID {
		return k.SessionID < other.SessionID
	}
	return k.DeviceID < other.DeviceID
}

// QueueState describes whether an item can be selected for another attempt.
type QueueState string

const (
	QueuePending QueueState = "pending"
	QueueBlocked QueueState = "blocked"
)

// FailureClass is the safe, finite classification persisted for a failed
// task. It intentionally does not contain the original error text.
type FailureClass string

const (
	FailureNone           FailureClass = ""
	FailureNetwork        FailureClass = "network"
	FailureUnknown        FailureClass = "unknown"
	FailureCredentials    FailureClass = "credentials"
	FailurePermission     FailureClass = "permission"
	FailureStorageFull    FailureClass = "storage-full"
	FailureSessionCorrupt FailureClass = "session-corrupt"
	FailureExcluded       FailureClass = "excluded"
)

// Validate checks whether a failure class is part of the stable queue enum.
func (f FailureClass) Validate() error {
	switch f {
	case FailureNone, FailureNetwork, FailureUnknown, FailureCredentials,
		FailurePermission, FailureStorageFull, FailureSessionCorrupt, FailureExcluded:
		return nil
	default:
		return fmt.Errorf("syncer: unsupported queue failure class %q", f)
	}
}

// Retryable reports whether this failure should receive exponential backoff.
func (f FailureClass) Retryable() bool {
	return f == FailureNetwork || f == FailureUnknown
}

// QueueItem is one durable pending task and its retry metadata.
type QueueItem struct {
	Key           QueueKey
	Attempt       uint32
	NextAttemptAt time.Time
	State         QueueState
	Failure       FailureClass
}

// Validate checks both the item fields and their state-machine invariants.
func (i QueueItem) Validate() error {
	if err := i.Key.Validate(); err != nil {
		return err
	}
	if err := i.Failure.Validate(); err != nil {
		return err
	}
	switch i.State {
	case QueuePending:
		if i.Failure == FailureNone {
			if i.Attempt != 0 || !i.NextAttemptAt.IsZero() {
				return errors.New("syncer: pending queue item without failure has retry state")
			}
		} else if !i.Failure.Retryable() {
			return errors.New("syncer: pending queue item has a terminal failure")
		} else if i.Attempt == 0 || i.NextAttemptAt.IsZero() {
			return errors.New("syncer: retryable pending queue item has incomplete retry state")
		}
	case QueueBlocked:
		if i.Failure == FailureNone || i.Failure.Retryable() {
			return errors.New("syncer: blocked queue item has no terminal failure")
		}
		if !i.NextAttemptAt.IsZero() {
			return errors.New("syncer: blocked queue item has a retry time")
		}
	default:
		return fmt.Errorf("syncer: unsupported queue item state %q", i.State)
	}
	return nil
}

// QueueSnapshot is the in-memory representation of the durable queue.
type QueueSnapshot struct {
	Items []QueueItem
}

// Validate checks all items and rejects duplicate task keys.
func (q QueueSnapshot) Validate() error {
	seen := make(map[QueueKey]struct{}, len(q.Items))
	for index, item := range q.Items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("syncer: queue item %d: %w", index, err)
		}
		if _, exists := seen[item.Key]; exists {
			return fmt.Errorf("syncer: duplicate queue item %s/%s/%s", item.Key.ProjectID, item.Key.SessionID, item.Key.DeviceID)
		}
		seen[item.Key] = struct{}{}
	}
	return nil
}

// Enqueue adds a task if it is not already present. Re-enqueueing an existing
// item is idempotent and does not clear a terminal block.
func (q *QueueSnapshot) Enqueue(key QueueKey) error {
	if q == nil {
		return errors.New("syncer: queue snapshot is required")
	}
	if err := key.Validate(); err != nil {
		return err
	}
	for _, item := range q.Items {
		if item.Key == key {
			return nil
		}
	}
	q.Items = append(q.Items, QueueItem{
		Key:   key,
		State: QueuePending,
	})
	return nil
}

// Item returns one queued task without changing the snapshot.
func (q QueueSnapshot) Item(key QueueKey) (QueueItem, error) {
	if err := key.Validate(); err != nil {
		return QueueItem{}, err
	}
	for _, item := range q.Items {
		if item.Key == key {
			return item, nil
		}
	}
	return QueueItem{}, ErrQueueItemMissing
}

// Reopen clears a terminal failure after the caller has revalidated the source
// data. Excluded and session-corrupt failures are source-data decisions;
// credential, permission and storage failures remain blocked until the user
// addresses the backend configuration.
func (q *QueueSnapshot) Reopen(key QueueKey, failure FailureClass) error {
	if q == nil {
		return errors.New("syncer: queue snapshot is required")
	}
	if err := key.Validate(); err != nil {
		return err
	}
	if failure != FailureExcluded && failure != FailureSessionCorrupt {
		return ErrQueueItemBlocked
	}
	for index, item := range q.Items {
		if item.Key != key {
			continue
		}
		if item.State != QueueBlocked || item.Failure != failure {
			return ErrQueueItemBlocked
		}
		q.Items[index] = QueueItem{Key: key, State: QueuePending}
		return nil
	}
	return ErrQueueItemMissing
}

// Complete removes a task after a successful durable sync.
func (q *QueueSnapshot) Complete(key QueueKey) error {
	if q == nil {
		return errors.New("syncer: queue snapshot is required")
	}
	if err := key.Validate(); err != nil {
		return err
	}
	for index, item := range q.Items {
		if item.Key != key {
			continue
		}
		copy(q.Items[index:], q.Items[index+1:])
		q.Items = q.Items[:len(q.Items)-1]
		return nil
	}
	return ErrQueueItemMissing
}

// RecordFailure updates a queued task after a failed attempt and returns the
// resulting durable item. The caller must persist the changed snapshot.
func (q *QueueSnapshot) RecordFailure(key QueueKey, failure FailureClass, now time.Time, policy RetryPolicy) (QueueItem, error) {
	if q == nil {
		return QueueItem{}, errors.New("syncer: queue snapshot is required")
	}
	if err := key.Validate(); err != nil {
		return QueueItem{}, err
	}
	if err := failure.Validate(); err != nil {
		return QueueItem{}, err
	}
	if failure == FailureNone {
		return QueueItem{}, errors.New("syncer: queue failure class is required")
	}
	for index, item := range q.Items {
		if item.Key != key {
			continue
		}
		if item.State == QueueBlocked {
			return QueueItem{}, ErrQueueItemBlocked
		}
		if failure.Retryable() {
			if now.IsZero() {
				return QueueItem{}, errors.New("syncer: retry time is required")
			}
			if err := policy.Validate(); err != nil {
				return QueueItem{}, err
			}
			if item.Attempt == ^uint32(0) {
				return QueueItem{}, ErrRetryExhausted
			}
			item.Attempt++
			delay, err := policy.Delay(item.Attempt)
			if err != nil {
				return QueueItem{}, err
			}
			item.NextAttemptAt = now.UTC().Add(delay)
			item.State = QueuePending
			item.Failure = failure
		} else {
			item.NextAttemptAt = time.Time{}
			item.State = QueueBlocked
			item.Failure = failure
		}
		q.Items[index] = item
		return item, nil
	}
	return QueueItem{}, ErrQueueItemMissing
}

// Due returns pending items whose retry time has arrived, sorted
// deterministically by time and then by opaque task key.
func (q QueueSnapshot) Due(now time.Time) []QueueItem {
	due := make([]QueueItem, 0, len(q.Items))
	for _, item := range q.Items {
		if item.State != QueuePending {
			continue
		}
		if !item.NextAttemptAt.IsZero() && item.NextAttemptAt.After(now) {
			continue
		}
		due = append(due, item)
	}
	sort.Slice(due, func(i, j int) bool {
		left, right := due[i], due[j]
		if left.NextAttemptAt.IsZero() != right.NextAttemptAt.IsZero() {
			return left.NextAttemptAt.IsZero()
		}
		if !left.NextAttemptAt.Equal(right.NextAttemptAt) {
			return left.NextAttemptAt.Before(right.NextAttemptAt)
		}
		return left.Key.less(right.Key)
	})
	return due
}

// RetryPolicy controls deterministic exponential backoff.
type RetryPolicy struct {
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

// DefaultRetryPolicy returns the product defaults for transient failures.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		BaseDelay: 5 * time.Second,
		MaxDelay:  time.Hour,
	}
}

// Validate checks a retry policy before it is used to update queue state.
func (p RetryPolicy) Validate() error {
	if p.BaseDelay <= 0 {
		return errors.New("syncer: retry base delay must be positive")
	}
	if p.MaxDelay < p.BaseDelay {
		return errors.New("syncer: retry max delay must not be smaller than base delay")
	}
	if p.MaxDelay > 365*24*time.Hour {
		return errors.New("syncer: retry max delay is too large")
	}
	return nil
}

// Delay computes the delay for a one-based failed-attempt number.
func (p RetryPolicy) Delay(attempt uint32) (time.Duration, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if attempt == 0 {
		return 0, errors.New("syncer: retry attempt must be positive")
	}
	delay := p.BaseDelay
	for count := uint32(1); count < attempt; count++ {
		if delay >= p.MaxDelay {
			return p.MaxDelay, nil
		}
		if delay > p.MaxDelay/2 {
			return p.MaxDelay, nil
		}
		delay *= 2
	}
	if delay > p.MaxDelay {
		return p.MaxDelay, nil
	}
	return delay, nil
}

// QueueStore persists queue metadata below a local configuration root.
type QueueStore struct {
	root        string
	processLock chan struct{}
}

// NewQueueStore validates a local queue root.
func NewQueueStore(root string) (QueueStore, error) {
	if strings.TrimSpace(root) == "" {
		return QueueStore{}, errors.New("syncer: queue state root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return QueueStore{}, fmt.Errorf("syncer: resolve queue state root: %w", err)
	}
	processLock := make(chan struct{}, 1)
	processLock <- struct{}{}
	return QueueStore{root: abs, processLock: processLock}, nil
}

func (s QueueStore) acquire(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	unlockProcess := func() {}
	if s.processLock != nil {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("syncer: acquire queue process lock: %w", ctx.Err())
		case <-s.processLock:
			unlockProcess = func() { s.processLock <- struct{}{} }
		}
	}
	_, err := s.filePath()
	if err != nil {
		unlockProcess()
		return nil, err
	}
	fileLock, err := AcquireLocalFileLock(ctx, filepath.Join(s.root, "queue.lock"))
	if err != nil {
		unlockProcess()
		return nil, err
	}
	return func() {
		_ = fileLock.Close()
		unlockProcess()
	}, nil
}

// Load reads and strictly validates the queue. A missing file is an empty
// queue, which is safe because queue metadata never establishes sync progress.
func (s QueueStore) Load(ctx context.Context) (QueueSnapshot, error) {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return QueueSnapshot{}, err
	}
	defer unlock()
	return s.load(ctx)
}

func (s QueueStore) load(ctx context.Context) (QueueSnapshot, error) {
	if ctx == nil {
		return QueueSnapshot{}, errors.New("syncer: context is required")
	}
	path, err := s.filePath()
	if err != nil {
		return QueueSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return QueueSnapshot{}, fmt.Errorf("syncer: load pending queue: %w", err)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return QueueSnapshot{}, nil
	}
	if err != nil {
		return QueueSnapshot{}, fmt.Errorf("syncer: read pending queue: %w", statePathSafe(err))
	}
	if err := ctx.Err(); err != nil {
		return QueueSnapshot{}, fmt.Errorf("syncer: load pending queue: %w", err)
	}

	var wire queueWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return QueueSnapshot{}, fmt.Errorf("%w: decode queue: %v", ErrInvalidQueue, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return QueueSnapshot{}, fmt.Errorf("%w: queue contains trailing JSON", ErrInvalidQueue)
	} else if !errors.Is(err, io.EOF) {
		return QueueSnapshot{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidQueue, err)
	}
	if wire.Version > queueVersion {
		return QueueSnapshot{}, fmt.Errorf("%w: version %d", ErrUnsupportedQueue, wire.Version)
	}
	if wire.Version != queueVersion {
		return QueueSnapshot{}, fmt.Errorf("%w: version %d", ErrInvalidQueue, wire.Version)
	}

	snapshot := QueueSnapshot{Items: make([]QueueItem, 0, len(wire.Items))}
	for index, item := range wire.Items {
		parsed, err := item.parse()
		if err != nil {
			return QueueSnapshot{}, fmt.Errorf("%w: item %d: %v", ErrInvalidQueue, index, err)
		}
		snapshot.Items = append(snapshot.Items, parsed)
	}
	if err := snapshot.Validate(); err != nil {
		return QueueSnapshot{}, fmt.Errorf("%w: %v", ErrInvalidQueue, err)
	}
	return snapshot, nil
}

// Save validates and atomically replaces the queue contents.
func (s QueueStore) Save(ctx context.Context, snapshot QueueSnapshot) error {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return s.save(ctx, snapshot)
}

func (s QueueStore) save(ctx context.Context, snapshot QueueSnapshot) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	path, err := s.filePath()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: save pending queue: %w", err)
	}

	items := append([]QueueItem(nil), snapshot.Items...)
	sortQueueItems(items)
	wire := queueWire{Version: queueVersion, Items: make([]queueItemWire, 0, len(items))}
	for _, item := range items {
		wire.Items = append(wire.Items, newQueueItemWire(item))
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("syncer: encode pending queue: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("syncer: create queue state directory: %w", statePathSafe(err))
	}
	if err := atomicfile.Write(path, func(w io.Writer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := w.Write(data)
		if err != nil {
			return fmt.Errorf("write pending queue: %w", err)
		}
		if written != len(data) {
			return fmt.Errorf("write pending queue: got %d bytes, expected %d", written, len(data))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("syncer: save pending queue: %w", statePathSafe(err))
	}
	return nil
}

// Update serializes a queue read-modify-write operation. It is used by the
// queued pusher when independent sessions are uploaded concurrently, so one
// worker cannot overwrite another worker's queue transition.
func (s QueueStore) Update(ctx context.Context, update func(*QueueSnapshot) error) error {
	if update == nil {
		return errors.New("syncer: queue update function is required")
	}
	unlock, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	snapshot, err := s.load(ctx)
	if err != nil {
		return err
	}
	if err := update(&snapshot); err != nil {
		return err
	}
	return s.save(ctx, snapshot)
}

// Enqueue loads the queue, adds a task idempotently, and saves it.
func (s QueueStore) Enqueue(ctx context.Context, key QueueKey) error {
	return s.Update(ctx, func(snapshot *QueueSnapshot) error {
		return snapshot.Enqueue(key)
	})
}

// Item loads one queued task without changing the queue.
func (s QueueStore) Item(ctx context.Context, key QueueKey) (QueueItem, error) {
	snapshot, err := s.Load(ctx)
	if err != nil {
		return QueueItem{}, err
	}
	return snapshot.Item(key)
}

// Reopen clears an excluded terminal task after the caller has independently
// revalidated its source session.
func (s QueueStore) Reopen(ctx context.Context, key QueueKey, failure FailureClass) error {
	return s.Update(ctx, func(snapshot *QueueSnapshot) error {
		return snapshot.Reopen(key, failure)
	})
}

// Complete removes a successfully synchronized task from the queue.
func (s QueueStore) Complete(ctx context.Context, key QueueKey) error {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	path, err := s.filePath()
	if err != nil {
		return err
	}
	snapshot, err := s.load(ctx)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrQueueFileMissing, ErrQueueItemMissing)
	} else if err != nil {
		return fmt.Errorf("syncer: inspect pending queue: %w", statePathSafe(err))
	}
	if err := snapshot.Complete(key); err != nil {
		return err
	}
	return s.save(ctx, snapshot)
}

// RecordFailure records a classified failure and persists its retry state.
func (s QueueStore) RecordFailure(ctx context.Context, key QueueKey, failure FailureClass, now time.Time, policy RetryPolicy) (QueueItem, error) {
	var item QueueItem
	err := s.Update(ctx, func(snapshot *QueueSnapshot) error {
		var err error
		item, err = snapshot.RecordFailure(key, failure, now, policy)
		return err
	})
	return item, err
}

// Due returns pending tasks eligible at the supplied time.
func (s QueueStore) Due(ctx context.Context, now time.Time) ([]QueueItem, error) {
	snapshot, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	return snapshot.Due(now), nil
}

type queueWire struct {
	Version int             `json:"version"`
	Items   []queueItemWire `json:"items"`
}

type queueItemWire struct {
	ProjectID     string `json:"projectId"`
	SessionID     string `json:"sessionId"`
	DeviceID      string `json:"deviceId"`
	Attempt       uint32 `json:"attempt"`
	NextAttemptAt string `json:"nextAttemptAt"`
	State         string `json:"state"`
	Failure       string `json:"failure"`
}

func newQueueItemWire(item QueueItem) queueItemWire {
	nextAttemptAt := ""
	if !item.NextAttemptAt.IsZero() {
		nextAttemptAt = item.NextAttemptAt.UTC().Format(time.RFC3339Nano)
	}
	return queueItemWire{
		ProjectID:     item.Key.ProjectID,
		SessionID:     item.Key.SessionID,
		DeviceID:      item.Key.DeviceID,
		Attempt:       item.Attempt,
		NextAttemptAt: nextAttemptAt,
		State:         string(item.State),
		Failure:       string(item.Failure),
	}
}

func (w queueItemWire) parse() (QueueItem, error) {
	key, err := NewQueueKey(w.ProjectID, w.SessionID, w.DeviceID)
	if err != nil {
		return QueueItem{}, err
	}
	nextAttemptAt, err := parseQueueTime(w.NextAttemptAt)
	if err != nil {
		return QueueItem{}, err
	}
	item := QueueItem{
		Key:           key,
		Attempt:       w.Attempt,
		NextAttemptAt: nextAttemptAt,
		State:         QueueState(w.State),
		Failure:       FailureClass(w.Failure),
	}
	if err := item.Validate(); err != nil {
		return QueueItem{}, err
	}
	return item, nil
}

func parseQueueTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid next attempt time: %v", err)
	}
	return parsed.UTC(), nil
}

func sortQueueItems(items []QueueItem) {
	sort.Slice(items, func(i, j int) bool {
		return items[i].Key.less(items[j].Key)
	})
}

func (s QueueStore) filePath() (string, error) {
	if strings.TrimSpace(s.root) == "" {
		return "", errors.New("syncer: queue state root is required")
	}
	return filepath.Join(s.root, "state", "v1", queueFile), nil
}
