//go:build !darwin && !linux

package main

import "errors"

func currentBoot() (BootInfo, error) {
	return BootInfo{}, errors.New(name + " supports macOS and Linux only")
}
