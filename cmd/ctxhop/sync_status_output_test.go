package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestWritePushSummaryReportsNoLocalSessions(t *testing.T) {
	var output bytes.Buffer
	if err := writePushSummary(&output, pushSummary{NoLocalSessions: true}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "pushed: 0, failed: 0, skipped: 0") {
		t.Fatalf("summary = %q", text)
	}
	if !strings.Contains(text, "no local sessions found for this project") {
		t.Fatalf("summary = %q", text)
	}
}

func TestPushSummaryLabelsDeadlineAsTimeout(t *testing.T) {
	var summary pushSummary
	summary.fail("remote-push", context.DeadlineExceeded)
	if !strings.Contains(summary.failureDetails, "class=timeout") {
		t.Fatalf("failure details = %q", summary.failureDetails)
	}
}

func TestNoRemotePullCheckReportIsExplicit(t *testing.T) {
	report := newPullCheckNoRemoteReport()
	if report.Result != pullCheckResultNoRemote {
		t.Fatalf("report = %+v", report)
	}
	var output bytes.Buffer
	if err := writePullCheckText(&output, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "result: no-remote-sessions") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestParseProjectDiscoverOptions(t *testing.T) {
	options, err := parseProjectOptions([]string{"discover", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if options.action != projectActionDiscover || !options.json {
		t.Fatalf("options = %+v", options)
	}
}
