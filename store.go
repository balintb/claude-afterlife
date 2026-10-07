package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	storeFormat         = 1
	keepSessionsPerBoot = 500
)

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Session struct {
	ID            string    `json:"session_id"`
	PID           int       `json:"pid"`
	Cwd           string    `json:"cwd"`
	Name          string    `json:"name,omitempty"`
	Kind          string    `json:"kind,omitempty"`
	ClaudeVersion string    `json:"claude_version,omitempty"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	FirstSeen     time.Time `json:"first_seen,omitzero"`
	LastSeen      time.Time `json:"last_seen,omitzero"`
}

// Boot is everything recorded during one boot of the machine, one file per boot.
// Sessions keeps every session seen during the boot, so a final snapshot taken
// after the terminal quit at shutdown cannot erase what was running a minute earlier.
type Boot struct {
	Format    int                `json:"format"`
	BootID    string             `json:"boot_id"`
	BootTime  time.Time          `json:"boot_time,omitzero"`
	UpdatedAt time.Time          `json:"updated_at"`
	Running   []string           `json:"running"`
	Sessions  map[string]Session `json:"sessions"`
}

func (a *App) bootsDir() string {
	return filepath.Join(a.StateDir, "boots")
}

func (a *App) bootPath(bootID string) string {
	return filepath.Join(a.bootsDir(), unsafeFileChars.ReplaceAllString(bootID, "_")+".json")
}

func loadBoot(path string) (*Boot, bool) {
	var boot Boot
	if !readJSON(path, &boot) || boot.Format != storeFormat || boot.Sessions == nil {
		return nil, false
	}
	return &boot, true
}

func (a *App) record() (*Boot, int, error) {
	info, err := a.Boot()
	if err != nil {
		return nil, 0, err
	}
	if info.ID == "" {
		return nil, 0, errors.New("cannot identify the current boot")
	}
	now := a.Now()
	path := a.bootPath(info.ID)
	boot, ok := loadBoot(path)
	if !ok {
		boot = &Boot{Format: storeFormat, BootID: info.ID, Sessions: map[string]Session{}}
	}
	live := a.liveSessions(info)
	running := make([]string, 0, len(live))
	for _, session := range live {
		session.FirstSeen = now
		if previous, seen := boot.Sessions[session.ID]; seen && !previous.FirstSeen.IsZero() {
			session.FirstSeen = previous.FirstSeen
		}
		session.LastSeen = now
		boot.Sessions[session.ID] = session
		running = append(running, session.ID)
	}
	trimSessions(boot)
	boot.BootTime = info.Time
	boot.UpdatedAt = now
	boot.Running = running
	if err := writePrivate(path, boot); err != nil {
		return nil, 0, err
	}
	a.pruneBoots(path)
	return boot, len(live), nil
}

func trimSessions(boot *Boot) {
	if len(boot.Sessions) <= keepSessionsPerBoot {
		return
	}
	sessions := make([]Session, 0, len(boot.Sessions))
	for _, session := range boot.Sessions {
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].LastSeen.After(sessions[j].LastSeen) })
	boot.Sessions = make(map[string]Session, keepSessionsPerBoot)
	for _, session := range sessions[:keepSessionsPerBoot] {
		boot.Sessions[session.ID] = session
	}
}

func writePrivate(path string, value any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

type bootFile struct {
	path     string
	modified time.Time
}

func (a *App) bootFiles() []bootFile {
	entries, _ := os.ReadDir(a.bootsDir())
	var files []bootFile
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, bootFile{filepath.Join(a.bootsDir(), entry.Name()), info.ModTime()})
	}
	return files
}

func (a *App) pruneBoots(current string) {
	files := a.bootFiles()
	sort.Slice(files, func(i, j int) bool { return files[i].modified.After(files[j].modified) })
	if len(files) <= a.KeepBoots {
		return
	}
	for _, file := range files[a.KeepBoots:] {
		if file.path != current {
			os.Remove(file.path)
		}
	}
}

func (a *App) allBoots() []*Boot {
	var boots []*Boot
	for _, file := range a.bootFiles() {
		if boot, ok := loadBoot(file.path); ok {
			boots = append(boots, boot)
		}
	}
	sort.SliceStable(boots, func(i, j int) bool { return boots[i].UpdatedAt.After(boots[j].UpdatedAt) })
	return boots
}

func selectBoot(boots []*Boot, which, currentID string) *Boot {
	for _, boot := range boots {
		switch which {
		case "current":
			if boot.BootID == currentID {
				return boot
			}
		case "previous":
			if boot.BootID != currentID {
				return boot
			}
		default:
			if len(which) > 0 && len(boot.BootID) >= len(which) && boot.BootID[:len(which)] == which {
				return boot
			}
		}
	}
	return nil
}
