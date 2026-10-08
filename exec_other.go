//go:build !unix

package main

import (
	"os"
	"os/exec"
)

func execReplace(dir string, argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
