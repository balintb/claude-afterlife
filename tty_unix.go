//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/term"
)

// isTerminal asks the kernel for the file's terminal settings, as isatty(3) does.
// A character device such as /dev/null is not a terminal.
func isTerminal(file *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), ioctlReadTermios, uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

func readSecretFromTerminal(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	secret, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(secret), err
}
