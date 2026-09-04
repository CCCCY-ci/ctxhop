package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallerPackRoundTrip(t *testing.T) {
	root := t.TempDir()
	stubPath := filepath.Join(root, "stub.exe")
	payloadPath := filepath.Join(root, "ctxhop.exe")
	outputPath := filepath.Join(root, "CtxHop-Setup.exe")
	stub := []byte("MZ test installer stub")
	payload := bytes.Repeat([]byte("ctxhop payload\x00"), 32)
	if err := os.WriteFile(stubPath, stub, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runPack([]string{"--stub", stubPath, "--payload", payloadPath, "--output", outputPath}); err != nil {
		t.Fatalf("runPack: %v", err)
	}
	packed, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(packed, stub) {
		t.Fatalf("packed installer does not preserve the stub prefix")
	}
	got, err := readInstallerPayloadPath(outputPath)
	if err != nil {
		t.Fatalf("readInstallerPayloadPath: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("unpacked payload = %q, want %q", got, payload)
	}
}

func TestInstallerPayloadChecksumRejectsTampering(t *testing.T) {
	root := t.TempDir()
	stubPath := filepath.Join(root, "stub.exe")
	payloadPath := filepath.Join(root, "ctxhop.exe")
	outputPath := filepath.Join(root, "CtxHop-Setup.exe")
	if err := os.WriteFile(stubPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payloadPath, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPack([]string{"--stub", stubPath, "--payload", payloadPath, "--output", outputPath}); err != nil {
		t.Fatalf("runPack: %v", err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(outputPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstallerPayloadPath(outputPath); err == nil {
		t.Fatal("tampered installer payload unexpectedly succeeded")
	}
}
