package environment

import "fmt"

// CaptureResult is the environment information an Agent adapter observed
// while reading one session. References are metadata-only observations;
// Components are the small, filtered bodies that the adapter knows how to
// capture safely.
type CaptureResult struct {
	References []Reference
	Components []ComponentContent
}

// Provider is the optional environment capability of an Agent adapter.
//
// Session, Git, and workspace synchronization remain Core capabilities and do
// not depend on this interface. An adapter only implements Provider for the
// environment formats it understands. The Core can therefore add a new Agent
// without adding Agent-name branches to push, preview, or apply flows.
type Provider interface {
	Name() string
	Capture(records [][]byte, version, agentHome, projectRoot, projectID string) CaptureResult
	Inspect(component Component, agentHome, projectRoot string) LocalComponentState
	Apply(content ComponentContent, agentHome, projectRoot, backupRoot string) (LocalComponentState, error)
}

// UnsupportedProvider is the safe fallback for an Agent whose environment
// format has no adapter implementation yet. It still records structured
// dependency evidence, but never guesses a local file or writes anything.
type UnsupportedProvider struct {
	Agent string
}

func (p UnsupportedProvider) Name() string {
	return p.Agent
}

func (p UnsupportedProvider) Capture(records [][]byte, version, _, _, _ string) CaptureResult {
	return CaptureResult{References: Discover(records, p.Agent, version)}
}

func (p UnsupportedProvider) Inspect(component Component, _, _ string) LocalComponentState {
	if err := component.Validate(); err != nil {
		return LocalComponentState{State: ComponentStateUnavailable, Reason: "remote component metadata is invalid"}
	}
	return LocalComponentState{
		State:  ComponentStateManual,
		Reason: fmt.Sprintf("the %s adapter has no safe automatic target for this environment component", displayAgent(p.Agent)),
	}
}

func (p UnsupportedProvider) Apply(content ComponentContent, _, _, _ string) (LocalComponentState, error) {
	if err := content.Validate(); err != nil {
		return LocalComponentState{State: ComponentStateFailed, Reason: "remote component content is invalid"}, err
	}
	return LocalComponentState{
		State:  ComponentStateManual,
		Reason: fmt.Sprintf("the %s adapter has no safe automatic target for this environment component", displayAgent(p.Agent)),
	}, nil
}

func displayAgent(agent string) string {
	if agent == "" {
		return "current Agent"
	}
	return agent
}
