package environment

import (
	"reflect"
	"testing"
)

func TestDiscoverOnlyUsesStructuredDependencyEvidence(t *testing.T) {
	records := [][]byte{
		[]byte("{\"type\":\"session_meta\",\"payload\":{\"cli_version\":\"0.148.0\"}}"),
		[]byte("{\"type\":\"response_item\",\"payload\":{\"type\":\"function_call\",\"name\":\"mcp__browser__open\"}}"),
		[]byte("{\"type\":\"event_msg\",\"payload\":{\"type\":\"skill_use\",\"name\":\"coding-guidelines\"}}"),
		[]byte("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"content\":\"mcp__not-a-server and skill-use text\"}}"),
	}
	got := Discover(records, "codex", "0.148.0")
	want := []Reference{
		{Kind: "mcp", Name: "browser", Portability: "platform-specific"},
		{Kind: "skill", Name: "coding-guidelines", Portability: "portable"},
		{Kind: "tool-requirement", Name: "codex", Version: "0.148.0", Portability: "platform-specific"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("references = %#v, want %#v", got, want)
	}
}

func TestDiscoverIgnoresUnrecognizedAndUnsafeObservations(t *testing.T) {
	records := [][]byte{
		[]byte("{\"type\":\"response_item\",\"payload\":{\"type\":\"function_call\",\"name\":\"shell\"}}"),
		[]byte("{\"type\":\"event_msg\",\"payload\":{\"type\":\"skill_use\",\"name\":\"C:/secret/skill\"}}"),
		[]byte("{\"type\":\"event_msg\",\"payload\":{\"type\":\"skill_use\",\"name\":\"\"}}"),
	}
	if got := Discover(records, "codex", ""); len(got) != 0 {
		t.Fatalf("references = %#v, want empty", got)
	}
}

func TestReferenceValidateRejectsPathsAndUnknownKinds(t *testing.T) {
	cases := []Reference{
		{Kind: "unknown", Name: "value", Portability: "portable"},
		{Kind: "skill", Name: "C:/secret", Portability: "portable"},
		{Kind: "skill", Name: "value", Portability: "unknown"},
	}
	for _, reference := range cases {
		if err := reference.Validate(); err == nil {
			t.Fatalf("Validate(%#v) succeeded", reference)
		}
	}
}
