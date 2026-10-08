//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// execReplace replaces this process with argv run in dir, so the terminal belongs
// to Claude Code exactly as if it had been started by hand.
func execReplace(dir string, argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	return syscall.Exec(path, argv, os.Environ())
}
