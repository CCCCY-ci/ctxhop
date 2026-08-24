package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestInstallerWelcomeShowsBrandAndNextStep(t *testing.T) {
	var output bytes.Buffer
	if err := writeInstallerWelcome(&output); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"CtxHop " + installerVersionLabel(version), "Installation complete", "Run: ctxhop init", "██████╗"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("welcome output does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestInstallerVersionLabelAddsPrefixOnce(t *testing.T) {
	for input, want := range map[string]string{
		"0.1.0":  "v0.1.0",
		"v0.1.0": "v0.1.0",
		"V0.1.0": "V0.1.0",
	} {
		if got := installerVersionLabel(input); got != want {
			t.Errorf("installerVersionLabel(%q) = %q, want %q", input, got, want)
		}
	}
}
