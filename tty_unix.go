//go:build darwin || linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal asks the kernel for the file's terminal settings, as isatty(3) does.
// A character device such as /dev/null is not a terminal.
func isTerminal(file *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), ioctlReadTermios, uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}
