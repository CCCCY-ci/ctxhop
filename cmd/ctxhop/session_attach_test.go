package main

import (
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

func TestNewNativeAttachBindingPreservesRootAndParentSemantics(t *testing.T) {
	opaque := func(ch byte) string { return strings.Repeat(string(ch), 26) }

	root := newNativeAttachBinding(
		opaque('a'), opaque('b'), opaque('c'), "codex", "native-root", opaque('d'), 1, nil,
	)
	if root.Origin.Kind != sessionhub.ReplicaOriginNative || len(root.Origin.BaseHeads) != 0 {
		t.Fatalf("root origin = %+v, want native without base heads", root.Origin)
	}
	if err := root.Validate(); err != nil {
		t.Fatalf("root binding is invalid: %v", err)
	}

	parent := opaque('e')
	continued := newNativeAttachBinding(
		opaque('a'), opaque('b'), opaque('c'), "codex", "native-continued", opaque('d'), 1, []string{parent},
	)
	if continued.Origin.Kind != sessionhub.ReplicaOriginSameAgentRestore || len(continued.Origin.BaseHeads) != 1 || continued.Origin.BaseHeads[0] != parent {
		t.Fatalf("continued origin = %+v, want same-agent parent provenance", continued.Origin)
	}
	if err := continued.Validate(); err != nil {
		t.Fatalf("continued binding is invalid: %v", err)
	}
}
