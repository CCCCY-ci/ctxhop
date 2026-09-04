//go:build windows

package main

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

func prepareSessionPickerOutput(output io.Writer) (func() error, error) {
	file, ok := output.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return func() error { return nil }, nil
	}
	handle := windows.Handle(file.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return nil, err
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return func() error { return nil }, nil
	}
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return nil, err
	}
	return func() error { return windows.SetConsoleMode(handle, mode) }, nil
}

func readPickerByte(input *os.File) (byte, error) {
	if input == nil {
		return 0, errors.New("picker input is required")
	}
	var value [1]byte
	var read uint32
	if err := windows.ReadFile(windows.Handle(input.Fd()), value[:], &read, nil); err != nil {
		return 0, err
	}
	if read != 1 {
		return 0, errors.New("picker input ended")
	}
	return value[0], nil
}

func readPickerByteTimeout(input *os.File, timeout time.Duration) (byte, bool, error) {
	if input == nil {
		return 0, false, errors.New("picker input is required")
	}
	milliseconds := timeout.Milliseconds()
	if milliseconds < 1 {
		milliseconds = 1
	}
	if milliseconds > int64(^uint32(0)-1) {
		milliseconds = int64(^uint32(0) - 1)
	}
	wait, err := windows.WaitForSingleObject(windows.Handle(input.Fd()), uint32(milliseconds))
	if err != nil {
		return 0, false, err
	}
	switch wait {
	case uint32(windows.WAIT_TIMEOUT):
		return 0, false, nil
	case windows.WAIT_OBJECT_0:
		value, readErr := readPickerByte(input)
		return value, true, readErr
	default:
		return 0, false, errors.New("picker input wait failed")
	}
}
