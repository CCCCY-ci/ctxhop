// Package adapter defines the per-Agent compatibility layer.
//
// Everything that knows how a specific AI coding agent stores its sessions on
// disk lives behind this interface: where the files are, how to read them
// without disturbing a running agent, which parts of them are bound to absolute
// paths, and how to write them back atomically.
//
// Nothing above this layer may assume any particular on-disk format. Adding
// support for a new agent means adding one implementation here and touching no
// sync logic (§8.2).
//
// Agents change their internal structures without notice, so implementations
// must degrade rather than guess: read leniently, write conservatively (§9.9).
package adapter

import (
	"context"
	"errors"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/environment"
)

// ErrNotInstalled is returned by Detect when the agent is absent from this
// machine. It is an expected outcome, not a failure: a missing agent must never
// produce an error message or affect any other agent (§9.2).
var ErrNotInstalled = errors.New("adapter: agent not installed")

// Compatibility expresses whether the adapter can safely handle the session
// structure it actually observed. Agent versions are diagnostic metadata only;
// a release is not limited merely because its version is new or unrecognised.
type Compatibility int

const (
	// CompatUnknown means compatibility has not been evaluated yet.
	CompatUnknown Compatibility = iota

	// CompatFull means the observed session fields are understood. All
	// operations are allowed.
	CompatFull

	// CompatLimited means the adapter has only partial structural evidence.
	// Backup may continue, but restoring requires explicit user confirmation
	// because writing is the operation that can destroy data.
	CompatLimited

	// CompatStopped means sessions cannot be parsed or fail validation. The
	// adapter performs no reads or writes and existing remote data is left
	// untouched.
	CompatStopped
)

// Installation describes a detected agent on the local machine.
type Installation struct {
	// Version is the agent version observed in its local session records, empty
	// if no record exposed one. It is diagnostic metadata only; it never decides
	// compatibility.
	Version string

	// VersionSource explains where Version came from. Adapters must not run an
	// agent executable just to obtain a version, so the built-in adapters use
	// "session-record" or "unavailable" here.
	VersionSource string

	// DataDir is the root directory holding the agent's local state.
	DataDir string

	// Compatibility is the level determined from the observed session fields.
	Compatibility Compatibility

	// CompatibilityReason explains the classification in user-facing terms and
	// is surfaced by `ctxhop doctor`. It must never contain paths, project
	// names or session content, so that users can paste diagnostics into public
	// issues (§9.9, BR-09).
	CompatibilityReason string
}

// SessionRef identifies a session discovered on disk, without its contents.
//
// Discovery is deliberately cheap: listing sessions must not require reading or
// decrypting session bodies.
type SessionRef struct {
	// Agent is the stable adapter name that owns this session, for example
	// "claude-code" or "codex". It is empty in legacy local/test values.
	Agent string

	// NativeID is the agent's own identifier for this session.
	NativeID string

	// ProjectPath is the absolute local path of the project this session
	// belongs to, as recorded by the agent.
	ProjectPath string

	// Title is a human-readable label derived locally for display. Agents
	// generally do not name sessions, so this is synthesised (§9.2.1). It is
	// content-derived and therefore encrypted before it ever leaves the machine.
	Title string

	// CreatedAt and UpdatedAt come from the agent's own records where
	// available, falling back to file timestamps.
	CreatedAt time.Time
	UpdatedAt time.Time

	// Size is the on-disk size of the session in bytes.
	Size int64

	// localPath is populated only by local discovery. It is intentionally
	// private so machine-specific paths cannot cross the metadata boundary.
	localPath string
}

// Adapter is the contract every supported agent implementation satisfies.
//
// Implementations must never lock, move or modify files the agent is using, and
// must leave the agent fully functional if CtxHop is removed (§4 P2, P5,
// BR-06, BR-13).
type Adapter interface {
	// Name returns a short, stable identifier such as "claude-code".
	Name() string

	// Detect locates the agent on this machine and classifies its version.
	// It returns ErrNotInstalled if the agent is absent.
	Detect(ctx context.Context) (Installation, error)

	// DiscoverSessions lists the sessions the agent currently holds.
	//
	// If projectPath is non-empty, only sessions belonging to that project are
	// returned.
	DiscoverSessions(ctx context.Context, projectPath string) ([]SessionRef, error)

	// ReadSession returns the full session content.
	//
	// The agent may be writing to the session concurrently, so implementations
	// must return a complete, parseable state and must never return a truncated
	// tail. Returning less data is always preferable to returning a partial
	// record (§9.2).
	ReadSession(ctx context.Context, ref SessionRef) ([]byte, error)

	// Rewrite translates a session captured on another machine into this
	// machine's path space.
	//
	// Cross-device restore is a structural transformation, not a file copy:
	// sessions embed absolute paths in several places, and the directory that
	// holds them may itself encode the project path. Implementations handle
	// separator, case-sensitivity and encoding differences between platforms,
	// and must leave paths outside the project root untouched rather than
	// guessing (§9.3, BR-10).
	//
	// It must fail rather than emit a partially rewritten session.
	Rewrite(ctx context.Context, session []byte, from, to ProjectPaths) ([]byte, error)

	// WriteSession installs a session into the agent's data directory so the
	// agent's own resume flow can find it.
	//
	// The write must be atomic: an interrupted call must leave either the
	// previous state or the new one, never a half-written session (BR-11).
	WriteSession(ctx context.Context, ref SessionRef, session []byte) error

	// TouchedFiles extracts the set of project-relative file paths this session
	// read or wrote.
	//
	// This set scopes the workspace consistency check. Comparing the whole
	// working tree would flag unrelated edits and train users to ignore the
	// warning, which would make the feature worthless; restricting the check to
	// files the session actually touched is what keeps it credible (§9.5).
	TouchedFiles(ctx context.Context, session []byte) ([]string, error)
}

// SessionLayout is the command-facing contract for an installed coding agent.
//
// The syncflow package only needs a small set of operations: discover local
// sessions, read one complete snapshot, and install a localised snapshot.
// Keeping this separate from Adapter preserves the richer compatibility
// contract above while allowing the CLI to support more than one agent without
// coupling it to a particular on-disk layout.
type SessionLayout interface {
	Name() string
	Detect(context.Context) (Installation, error)
	DiscoverSessions(projectRoot string) ([]SessionRef, error)
	ReadSession(SessionRef) (SessionData, error)
	WriteSession(projectRoot, sessionID string, records [][]byte) error
	ReplaceSession(projectRoot, sessionID string, records [][]byte) error
	TouchedFiles(records [][]byte, projectRoot string) []FileAccess
}

// AgentSessions pairs an installed agent with the sessions it owns for one
// project. Sessions from different agents remain separate at this boundary;
// the remote summary carries the agent name so resume can select the same
// layout on another device.
type AgentSessions struct {
	Layout       SessionLayout
	Installation Installation
	Sessions     []SessionRef
}

// ProjectPaths pairs a project's root directory with the identity the agent
// uses to refer to it, so Rewrite can map one machine's view onto another's.
type ProjectPaths struct {
	// Root is the absolute path of the project root on that machine.
	Root string

	// AgentProjectKey is however the agent names this project internally, for
	// example a directory name derived from the encoded absolute path.
	AgentProjectKey string
}

// HookInstaller is implemented by adapters whose agent offers a session
// lifecycle hook.
//
// Where available, a hook is preferred over filesystem watching: it fires at a
// well-defined moment, needs no resident process, and the user can remove it to
// uninstall CtxHop completely (spec §8.5, §4 P5).
type HookInstaller interface {
	// InstallHook registers a hook that runs `ctxhop push` when a session
	// ends. It must be idempotent and must not disturb hooks installed by
	// anyone else.
	InstallHook(executable string) error

	// RemoveHook removes only the hook this tool installed.
	RemoveHook() error

	// HookInstalled reports whether our hook is currently registered.
	HookInstalled() (bool, error)
}

// EnvironmentCapable is an optional Adapter capability. Core session,
// workspace, Git, and no-Git synchronization never requires it; it is only
// used for environment components whose on-disk format belongs to one Agent.
type EnvironmentCapable interface {
	Environment() environment.Provider
}

// EnvironmentFor returns the environment provider owned by a layout. Layouts
// that do not expose one receive a fail-closed provider instead of causing the
// command layer to branch on an Agent name.
func EnvironmentFor(layout SessionLayout) environment.Provider {
	if layout != nil {
		if capable, ok := layout.(EnvironmentCapable); ok {
			if provider := capable.Environment(); provider != nil {
				return provider
			}
		}
		return environment.UnsupportedProvider{Agent: layout.Name()}
	}
	return environment.UnsupportedProvider{}
}
