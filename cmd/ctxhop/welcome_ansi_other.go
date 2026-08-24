//go:build !windows

package main

import "os"

func enableInstallerWelcomeANSI(_ *os.File) bool {
	return true
}
