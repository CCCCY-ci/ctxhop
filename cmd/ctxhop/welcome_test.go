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

	for _, want := range []string{"CtxHop " + version, "Installation complete.", "ctxhop init"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("welcome output does not contain %q:\n%s", want, output.String())
		}
	}
}
