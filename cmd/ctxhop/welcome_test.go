package main

import (
	"bytes"
	"fmt"
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
	if strings.Contains(output.String(), "\x1b[") {
		t.Fatalf("non-terminal welcome output contains ANSI escapes:\n%q", output.String())
	}
}

func TestInstallerBannerUsesApprovedGrayGradient(t *testing.T) {
	output := renderInstallerBanner(true)
	for _, gray := range installerWordmarkGray {
		want := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", gray, gray, gray)
		if !strings.Contains(output, want) {
			t.Errorf("gradient banner does not contain %q", want)
		}
	}
	if count := strings.Count(output, ansiReset); count != len(installerWordmarkGray) {
		t.Errorf("gradient banner reset count = %d, want %d", count, len(installerWordmarkGray))
	}
	if got := renderInstallerBanner(false); got != ctxhopASCIILogo {
		t.Error("plain banner changed when gradient was disabled")
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
