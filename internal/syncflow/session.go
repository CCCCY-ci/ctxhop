// Package syncflow composes Agent adapter output with the format-independent
// syncer. It is deliberately separate from both layers: adapters know local
// files and path fields, while syncer knows canonical records and objects.
package syncflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

var (
	// ErrInvalidSessionSnapshot reports data that did not come from a strict
	// complete adapter read.
	ErrInvalidSessionSnapshot = errors.New("syncflow: invalid session snapshot")

	// ErrSessionNotPushable reports a session the adapter compatibility policy
	// has stopped before any remote operation can begin.
	ErrSessionNotPushable = errors.New("syncflow: session is not safe to push")

	// ErrInvalidPathSpace reports missing roots that would make canonicalization
	// leave machine-local path values in the remote record stream.
	ErrInvalidPathSpace = errors.New("syncflow: incomplete path space")
)

// CanonicalStream is the immutable-by-convention record stream handed to the
// syncer after adapter path processing.
type CanonicalStream struct {
	// Records contains complete canonical JSON records in source order.
	Records [][]byte

	// DroppedTail reports that the adapter omitted an unterminated final line.
	// The complete prefix remains safe to push; the next read can pick up the
	// agent's eventual completed line.
	DroppedTail bool

	// Compatibility and CompatibilityReason are safe adapter diagnostics. They
	// contain no paths or session content.
	Compatibility       adapter.Compatibility
	CompatibilityReason string

	// UnknownPathFields is already redacted and sorted by the adapter.
	UnknownPathFields []string
}

// CanonicalizeSession converts a strict adapter snapshot into canonical
// records. It performs no remote or filesystem I/O.
func CanonicalizeSession(data adapter.SessionData, space adapter.PathSpace, installation adapter.Installation) (CanonicalStream, error) {
	if data.Skipped != 0 {
		return CanonicalStream{}, fmt.Errorf("%w: lenient input skipped %d record(s)", ErrInvalidSessionSnapshot, data.Skipped)
	}
	if strings.TrimSpace(space.ProjectRoot) == "" || strings.TrimSpace(space.AgentHome) == "" {
		return CanonicalStream{}, ErrInvalidPathSpace
	}

	canonicalizer := adapter.NewCanonicalizer(space)
	records := make([][]byte, 0, len(data.Records))
	for i, record := range data.Records {
		if isWorkspaceContextRecord(record) {
			continue
		}
		canonical, err := canonicalizer.Record(record)
		if err != nil {
			cause := errors.Join(ErrInvalidSessionSnapshot, err)
			return CanonicalStream{}, fmt.Errorf("syncflow: canonicalize record %d: %w", i+1, cause)
		}
		records = append(records, append([]byte(nil), canonical...))
	}

	findings := canonicalizer.UnknownPathFields()
	level, findingReason := adapter.GradeSession(installation.Compatibility, findings)
	reason := installation.CompatibilityReason
	if findingReason != "" {
		reason = findingReason
	}
	if level == adapter.CompatStopped {
		if reason == "" {
			reason = "adapter compatibility policy stopped this session"
		}
		return CanonicalStream{}, fmt.Errorf("%w: %s", ErrSessionNotPushable, reason)
	}

	return CanonicalStream{
		Records:             records,
		DroppedTail:         data.DroppedTail,
		Compatibility:       level,
		CompatibilityReason: reason,
		UnknownPathFields:   append([]string(nil), findings...),
	}, nil
}

// Push publishes the canonical stream through a durable append executor.
func (s CanonicalStream) Push(ctx context.Context, executor syncer.AppendExecutor, cursor syncer.PushCursor) (syncer.PushCursor, error) {
	if ctx == nil {
		return syncer.PushCursor{}, errors.New("syncflow: context is required")
	}
	if err := ctx.Err(); err != nil {
		return syncer.PushCursor{}, fmt.Errorf("syncflow: push session: %w", err)
	}
	return executor.Execute(ctx, cursor, s.Records)
}
