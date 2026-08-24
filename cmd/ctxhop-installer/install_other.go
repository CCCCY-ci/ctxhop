//go:build !windows

package main

import "errors"

func installPayload([]byte) (string, error) {
	return "", errors.New("the release installer is only supported on Windows")
}

func reportInstallerFailure(err error) {
	// The packer runs on non-Windows hosts, but the installer payload itself is
	// intentionally Windows-only. Keep a normal stderr path for accidental use.
	println("ctxhop-installer: " + err.Error())
}

func reportInstallerSuccess(string) {}
