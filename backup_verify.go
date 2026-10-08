package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
)

type verifyReport struct {
	Files    int
	Sessions int
	Verified int
	Archived int
	Pending  []string
	Problems []string
}

func (r verifyReport) safe() bool {
	return len(r.Pending) == 0 && len(r.Problems) == 0
}

// verifyBackup rebuilds every file of this machine's backup from st, checks every
// hash, and compares the result with what is on disk now. Anything on disk that is
// not in the backup, or newer than it, is reported as pending.
func (a *App) verifyBackup(config *backupConfig, name string, st store, key backupKey) (verifyReport, error) {
	var report verifyReport
	index, err := st.readIndex(config.MachineID, key)
	if err != nil {
		return report, err
	}
	sources, err := a.backupSources(config, name)
	if err != nil {
		return report, err
	}
	bySource := map[string]sourceFile{}
	for _, source := range sources {
		bySource[source.storePath] = source
	}
	inIndex := map[string]bool{}
	for _, entry := range index.Files {
		inIndex[entry.Path] = true
		report.Files++
		if isSessionPath(entry.Path) {
			report.Sessions++
		}
		display := displayStorePath(entry.Path)
		if err := st.assemble(entry, key.identities, io.Discard); err != nil {
			report.Problems = append(report.Problems, fmt.Sprintf("%s: %v", display, err))
			continue
		}
		source, onDisk := bySource[entry.Path]
		if entry.Deleted || !onDisk {
			report.Archived++
			report.Verified++
			continue
		}
		if pending := compareWithDisk(entry, source); pending != "" {
			report.Pending = append(report.Pending, display+": "+pending)
			continue
		}
		report.Verified++
	}
	for _, source := range sources {
		if !inIndex[source.storePath] {
			report.Pending = append(report.Pending, displayStorePath(source.storePath)+": not backed up yet")
		}
	}
	sort.Strings(report.Pending)
	sort.Strings(report.Problems)
	return report, nil
}

func compareWithDisk(entry fileEntry, source sourceFile) string {
	if source.localPath == "" {
		if sha256Hex(source.content) != entry.SHA256 {
			return "changed since the backup"
		}
		return ""
	}
	file, err := os.Open(source.localPath)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "unreadable: " + err.Error()
	}
	if info.Size() < entry.Size {
		return "changed since the backup"
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(file, 0, entry.Size)); err != nil {
		return "unreadable: " + err.Error()
	}
	if hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return "changed since the backup"
	}
	if entry.Kind == kindWhole {
		if info.Size() != entry.Size {
			return "changed since the backup"
		}
		return ""
	}
	end, err := lastLineEnd(file, entry.Size, info.Size())
	if err != nil {
		return "unreadable: " + err.Error()
	}
	if end > entry.Size {
		return "has new lines since the backup"
	}
	return ""
}
