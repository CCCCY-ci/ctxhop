package config

import (
	"errors"
	"fmt"
	"io/fs"
)

// pathSafe strips the filename out of a filesystem error while keeping its
// cause, so errors.Is still answers correctly.
//
// A *fs.PathError renders as "open C:\Users\someone\AppData\...: ...". These
// errors reach the user, and an absolute path names both the person and their
// machine's layout (BR-09, code_style §5.2).
//
// The project layer carries the same helper. They are kept apart rather than
// shared through one of these two packages, which are siblings; once a third
// caller needs it, it belongs in a utility package of its own alongside
// atomicfile.
func pathSafe(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}
