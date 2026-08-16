package main

import (
	"context"
	"errors"

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/diagnostic"
)

// recordCommandFailure records only a safe command label and error class.
// Recording is best effort: a diagnostic failure must never hide the command
// error that the user needs to see.
func recordCommandFailure(args []string, err error) {
	if err == nil {
		return
	}
	dir, dirErr := config.Dir()
	if dirErr != nil {
		return
	}
	_ = diagnostic.Record(dir, safeCommandLabel(args), commandErrorClass(err))
}

func safeCommandLabel(args []string) string {
	if len(args) == 0 {
		return "unknown"
	}
	for _, command := range commands {
		if command.name == args[0] {
			return command.name
		}
	}
	return "unknown"
}

func commandErrorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, config.ErrNotInitialised):
		return "not-initialized"
	case errors.Is(err, config.ErrNoSecrets):
		return "credentials-unavailable"
	case errors.Is(err, config.ErrPartialEnvironment):
		return "credentials-incomplete"
	default:
		return "command-failed"
	}
}
