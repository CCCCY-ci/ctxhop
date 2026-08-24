package main

import (
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/gitstate"
)

func TestGitApplyRetryBlockedOnlyWhenPreflightIsUnsafe(t *testing.T) {
	record := &gitstate.ApplyRecord{ManualCleanupRequired: true}
	cases := []struct {
		name   string
		record *gitstate.ApplyRecord
		status string
		want   bool
	}{
		{name: "unsafe previous apply", record: record, status: gitstate.ApplyConflict, want: true},
		{name: "clean after manual cleanup", record: record, status: gitstate.ApplyReady, want: false},
		{name: "no previous apply", status: gitstate.ApplyConflict, want: false},
		{name: "previous record without cleanup", record: &gitstate.ApplyRecord{}, status: gitstate.ApplyConflict, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := gitApplyRetryBlocked(test.record, test.status); got != test.want {
				t.Fatalf("gitApplyRetryBlocked() = %t, want %t", got, test.want)
			}
		})
	}
}
