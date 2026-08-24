package main

import (
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/logging"
)

var commandLogger *logging.Logger

func startCommandLogging(args []string) {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	commandLogger = logging.New(dir)
	commandLogger.Info("command_started", "command", safeCommandLabel(args))
}

func finishCommandLogging(args []string, err error) {
	if commandLogger == nil {
		return
	}
	command := safeCommandLabel(args)
	if err == nil {
		commandLogger.Info("command_finished", "command", command, "result", "success")
		return
	}
	commandLogger.Error(
		"command_finished",
		"command", command,
		"result", "failed",
		"class", commandErrorClass(err),
		"error", logging.SanitizeError(err),
	)
}

func logPushFailure(agent, stage, class string, err error) {
	if commandLogger == nil {
		return
	}
	args := []any{
		"stage", stage,
		"class", class,
		"error", logging.SanitizeError(err),
	}
	if agent != "" {
		args = append(args, "agent", agent)
	}
	commandLogger.Error("push_failure", args...)
}

func logPushFinished(summary pushSummary, workspace bool) {
	if commandLogger == nil {
		return
	}
	result := "success"
	if summary.Failed != 0 {
		result = "partial-failure"
	}
	commandLogger.Info(
		"push_finished",
		"result", result,
		"pushed", summary.Pushed,
		"failed", summary.Failed,
		"skipped", summary.Skipped,
		"workspace", workspace,
		"no_local_sessions", summary.NoLocalSessions,
	)
}

func logPushSessionFinished(agent string, summary pushSummary, duration time.Duration) {
	if commandLogger == nil {
		return
	}
	result := "success"
	if summary.Failed != 0 {
		result = "failed"
	}
	commandLogger.Info(
		"session_push_finished",
		"agent", agent,
		"result", result,
		"duration_ms", duration.Milliseconds(),
	)
}

func currentLogPath(configDir string) string {
	return logging.CurrentPath(configDir, time.Now())
}
