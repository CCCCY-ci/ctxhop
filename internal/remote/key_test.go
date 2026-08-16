package remote

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateKeyAccepts(t *testing.T) {
	valid := []string{
		"v1/projects/abc123/sessions/def456/dev789/000001",
		"v1/devices/abc",
		"single",
		"v1/with-dash_and.dot/x",
		"v1/unicode-名字/x",
		strings.Repeat("a", maxKeyLen),
	}
	for _, key := range valid {
		if err := ValidateKey(key); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", key, err)
		}
	}
}

func TestValidateKeyRejects(t *testing.T) {
	invalid := map[string]string{
		"empty":               "",
		"absolute":            "/v1/a",
		"parent traversal":    "v1/../../escape",
		"bare parent":         "..",
		"current dir":         "v1/./a",
		"empty segment":       "v1//a",
		"trailing separator":  "v1/a/",
		"backslash":           `v1\a`,
		"drive letter":        "C:/v1/a",
		"control character":   "v1/a\x00b",
		"newline":             "v1/a\nb",
		"trailing dot":        "v1/a.",
		"trailing space":      "v1/a ",
		"leading space":       "v1/ a",
		"longer than the max": strings.Repeat("a", maxKeyLen+1),
	}
	for name, key := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateKey(key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("ValidateKey(%q) = %v, want ErrInvalidKey", key, err)
			}
		})
	}
}

func TestValidateKeyRejectsBackslashRatherThanTranslating(t *testing.T) {
	// Translating it would make the same logical key name different objects on
	// Windows and elsewhere, so a session pushed from one machine would be
	// invisible to the other.
	if err := ValidateKey(`v1\projects\a`); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("got %v, want ErrInvalidKey", err)
	}
}

func TestValidatePrefix(t *testing.T) {
	valid := []string{
		"",
		"v1/",
		"v1/projects/a/sessions/s/dev/0000",
		"v1",
	}
	for _, prefix := range valid {
		if err := ValidatePrefix(prefix); err != nil {
			t.Errorf("ValidatePrefix(%q) = %v, want nil", prefix, err)
		}
	}

	invalid := []string{
		"/absolute",
		"../escape",
		"v1/../x",
		`v1\a`,
		"C:/drive",
		"v1/\x01",
		strings.Repeat("a", maxKeyLen+1),
	}
	for _, prefix := range invalid {
		if err := ValidatePrefix(prefix); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ValidatePrefix(%q) = %v, want ErrInvalidKey", prefix, err)
		}
	}
}
