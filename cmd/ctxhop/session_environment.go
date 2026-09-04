package main

import (
	"context"
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/CCCCY-ci/ctxhop/internal/environment"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

type nativeEnvironmentPublication struct {
	EnvironmentID   string
	EnvironmentRefs []string
	Components      []sessionhub.EnvironmentComponentRef
}

// publishNativeEnvironmentComponents stores only the filtered component
// bodies observed by the local Agent. The shared component pool is keyed by
// Hub and fingerprint, while the Session contribution later points to one
// immutable attachment describing the exact component set.
func publishNativeEnvironmentComponents(ctx context.Context, identifierKey []byte, hubKey, projectKey, deviceID string, store remote.Remote, public *ecdh.PublicKey, identities []*ecdh.PrivateKey, components []environment.ComponentContent) (nativeEnvironmentPublication, error) {
	components = environment.NormalizeComponentContents(components)
	if len(components) == 0 {
		return nativeEnvironmentPublication{}, nil
	}
	layout, err := syncer.NewEnvironmentHubLayout(hubKey)
	if err != nil {
		return nativeEnvironmentPublication{}, err
	}
	refs := make([]sessionhub.EnvironmentComponentRef, 0, len(components))
	for _, content := range components {
		component := content.Component
		if component.Scope == "project" {
			component.ProjectID = projectKey
			content.Component.ProjectID = projectKey
		}
		componentKey, err := sessionhub.DeriveEnvironmentKey(identifierKey, hubKey, component.Fingerprint)
		if err != nil {
			return nativeEnvironmentPublication{}, fmt.Errorf("derive environment component identity: %w", err)
		}
		if err := syncer.PutEnvironmentComponentWithIdentities(ctx, store, public, layout, componentKey, deviceID, content, identities); err != nil {
			return nativeEnvironmentPublication{}, err
		}
		refs = append(refs, sessionhub.EnvironmentComponentRef{
			Kind:        component.Kind,
			Name:        component.Name,
			Scope:       component.Scope,
			ProjectID:   component.ProjectID,
			Fingerprint: component.Fingerprint,
			Portability: component.Portability,
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		return environmentComponentRefKey(refs[i]) < environmentComponentRefKey(refs[j])
	})
	encoded, err := json.Marshal(refs)
	if err != nil {
		return nativeEnvironmentPublication{}, fmt.Errorf("encode environment attachment identity: %w", err)
	}
	digestInput := append([]byte("ctxhop/environment-attachment/v1\x00"), encoded...)
	digest := sessionhub.DigestBytes(digestInput)
	environmentID := hex.EncodeToString(digest[:])
	return nativeEnvironmentPublication{
		EnvironmentID:   environmentID,
		EnvironmentRefs: []string{environmentID},
		Components:      refs,
	}, nil
}

func publishNativeEnvironmentAttachment(ctx context.Context, store remote.Remote, public *ecdh.PublicKey, identities []*ecdh.PrivateKey, publication nativeReplicaPublication, deviceID string, environmentPublication nativeEnvironmentPublication, contributionID string) error {
	if environmentPublication.EnvironmentID == "" || len(environmentPublication.Components) == 0 {
		return nil
	}
	if contributionID == "" {
		return errors.New("session hub: environment attachment has no Contribution identity")
	}
	sessionLayout, err := publication.Layout.SessionLayout()
	if err != nil {
		return fmt.Errorf("prepare environment attachment layout: %w", err)
	}
	attachment := sessionhub.EnvironmentAttachment{
		Version:                sessionhub.ModelVersion,
		EnvironmentID:          environmentPublication.EnvironmentID,
		SessionID:              publication.Descriptor.SessionID,
		SourceAgent:            publication.Descriptor.Source.Agent,
		ObservedAtContribution: contributionID,
		Components:             append([]sessionhub.EnvironmentComponentRef(nil), environmentPublication.Components...),
	}
	return syncer.PutEnvironmentAttachmentWithIdentities(ctx, store, public, sessionLayout, environmentPublication.EnvironmentID, deviceID, attachment, identities)
}

func environmentComponentRefKey(ref sessionhub.EnvironmentComponentRef) string {
	return ref.Kind + "\x00" + ref.Name + "\x00" + ref.Scope + "\x00" + ref.ProjectID + "\x00" + ref.Fingerprint
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
