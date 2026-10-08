//go:build !darwin && !linux

package main

import (
	"errors"
	"os"
)

func isTerminal(*os.File) bool {
	return false
}

func readSecretFromTerminal(string) (string, error) {
	return "", errors.New("reading a hidden private key is not supported here; pass -identity <file>")
}
