//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func currentBoot() (BootInfo, error) {
	var info BootInfo
	if data, err := os.ReadFile("/proc/stat"); err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			if rest, ok := strings.CutPrefix(line, "btime "); ok {
				if seconds, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
					info.Time = time.Unix(seconds, 0)
				}
				break
			}
		}
	}
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		info.ID = strings.TrimSpace(string(data))
	}
	if info.ID == "" && !info.Time.IsZero() {
		info.ID = fmt.Sprintf("boottime-%d", info.Time.Unix())
	}
	if info.ID == "" {
		return info, errors.New("cannot identify the current boot: /proc is not readable")
	}
	return info, nil
}
