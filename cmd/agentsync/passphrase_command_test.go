package main

import (
	"bytes"
	"testing"
)

func TestWriteRecoveryResetReminder(t *testing.T) {
	var prompt bytes.Buffer
	if err := writeRecoveryResetReminder(&prompt); err != nil {
		t.Fatalf("writeRecoveryResetReminder() error = %v", err)
	}
	if got, want := prompt.String(), recoveryResetReminder+"\n"; got != want {
		t.Fatalf("reminder = %q, want %q", got, want)
	}
}

func TestWriteRecoveryResetReminderRejectsMissingPrompt(t *testing.T) {
	if err := writeRecoveryResetReminder(nil); err == nil || err.Error() != "passphrase: prompt output is required" {
		t.Fatalf("writeRecoveryResetReminder(nil) error = %v", err)
	}
}
