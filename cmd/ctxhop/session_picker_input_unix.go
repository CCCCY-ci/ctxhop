//go:build !windows

package main

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func prepareSessionPickerOutput(io.Writer) (func() error, error) {
	return func() error { return nil }, nil
}

func readPickerByte(input *os.File) (byte, error) {
	if input == nil {
		return 0, errors.New("picker input is required")
	}
	var value [1]byte
	for {
		read, err := unix.Read(int(input.Fd()), value[:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if read != 1 {
			return 0, errors.New("picker input ended")
		}
		return value[0], nil
	}
}

func readPickerByteTimeout(input *os.File, timeout time.Duration) (byte, bool, error) {
	if input == nil {
		return 0, false, errors.New("picker input is required")
	}
	milliseconds := int(timeout / time.Millisecond)
	if milliseconds < 1 {
		milliseconds = 1
	}
	fd := int(input.Fd())
	for {
		pollFD := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		ready, err := unix.Poll(pollFD, milliseconds)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		if ready == 0 {
			return 0, false, nil
		}
		value, readErr := readPickerByte(input)
		return value, true, readErr
	}
}
