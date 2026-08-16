package main

import (
	"context"
	"errors"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/diagnostic"
)

func TestRecordCommandFailureStoresOnlySafeClassAndCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSYNC_CONFIG_DIR", dir)
	recordCommandFailure([]string{"resume", `C:\private\session`}, errors.New(`failed at C:\private\secret`))
	events, err := diagnostic.Recent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Command != "resume" || events[0].Class != "command-failed" {
		t.Fatalf("events = %+v", events)
	}
}

func TestCommandFailureClassifiesStableSentinels(t *testing.T) {
	if got := commandErrorClass(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("deadline class = %q", got)
	}
	if got := commandErrorClass(context.Canceled); got != "canceled" {
		t.Fatalf("canceled class = %q", got)
	}
	if got := safeCommandLabel([]string{`C:\private`}); got != "unknown" {
		t.Fatalf("unsafe command label = %q", got)
	}
}
