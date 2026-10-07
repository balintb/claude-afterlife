//go:build darwin

package main

import (
	"encoding/binary"
	"errors"
	"syscall"
	"time"
)

func currentBoot() (BootInfo, error) {
	id, err := syscall.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return BootInfo{}, err
	}
	if id == "" {
		return BootInfo{}, errors.New("kern.bootsessionuuid is empty")
	}
	info := BootInfo{ID: id}
	// kern.boottime is a struct timeval; its first eight bytes are the seconds.
	if raw, err := syscall.Sysctl("kern.boottime"); err == nil && len(raw) >= 8 {
		info.Time = time.Unix(int64(binary.LittleEndian.Uint64([]byte(raw[:8]))), 0)
	}
	return info, nil
}
