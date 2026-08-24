package config

// Summary is the shareable view of a configuration.
//
// It is a separate type rather than a blanked-out Config for two reasons. A
// Config with its fields emptied could be handed to Save and would overwrite
// the real one; and a type that holds no free-text field cannot be made to leak
// by someone adding a field later and forgetting to clear it.
//
// Nothing here comes from the user. A bucket name gives away an employer, a
// device name is often a person's name, and an absolute path gives away a
// username - so none of them are represented at all, only whether they are set
// and how many there are. That turns "remember to redact" into "there is
// nothing to redact" (BR-09, spec §3).
type Summary struct {
	Version int

	RemoteType       string
	RemoteConfigured bool
	EndpointSet      bool

	DeviceIdentified bool
	DeviceMode       string
	IdentityPinned   bool

	BoundProjects    int
	ExcludedProjects int
	PushOnlyProjects int

	Agents map[string]AgentState
}

// Summarise renders the configuration for output the user can paste into a
// public issue without reading it first (BR-09).
func (c *Config) Summarise() Summary {
	if c == nil {
		return Summary{}
	}

	s := Summary{
		Version:          c.Version,
		RemoteType:       c.Remote.Type,
		RemoteConfigured: c.Remote.Bucket != "" || c.Remote.Path != "",
		EndpointSet:      c.Remote.Endpoint != "",
		DeviceIdentified: c.Device.ID != "",
		DeviceMode:       string(c.Device.Mode.Effective()),
		IdentityPinned:   len(c.IdentityPublic) > 0,
		BoundProjects:    len(c.Projects.Bindings),
		ExcludedProjects: len(c.Projects.Excluded),
		PushOnlyProjects: len(c.Projects.PushOnly),
	}

	// Agent state is booleans the user chose, not values they typed, so it can
	// be shown as it is - and it is the part a bug report actually needs.
	if len(c.Agents) > 0 {
		s.Agents = make(map[string]AgentState, len(c.Agents))
		for name, state := range c.Agents {
			s.Agents[name] = state
		}
	}
	return s
}
