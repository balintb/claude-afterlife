package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	statusReopen            = "reopen"
	statusInvalid           = "invalid entry"
	statusRunning           = "already running"
	statusMissingDirectory  = "missing directory"
	statusMissingTranscript = "missing transcript"
)

type selection struct {
	Recent            time.Duration
	All               bool
	IncludeBackground bool
}

func (s selection) includes(session Session) bool {
	return s.IncludeBackground || session.Kind == "" || session.Kind == "interactive"
}

type processKey struct {
	pid     int
	started int64
}

func keyOf(session Session) processKey {
	return processKey{session.PID, session.StartedAt.UnixMilli()}
}

type runningSet struct {
	ids   map[string]bool
	procs map[processKey]bool
}

func (a *App) runningNow(boot BootInfo) runningSet {
	running := runningSet{ids: map[string]bool{}, procs: map[processKey]bool{}}
	for _, session := range a.liveSessions(boot) {
		running.ids[session.ID] = true
		running.procs[keyOf(session)] = true
	}
	return running
}

type restoreGroup struct {
	Title    string
	Sessions []Session
}

// shutdownGroup returns the sessions of an earlier boot that were still running
// within Recent of its final snapshot, i.e. the ones the reboot took down.
func (a *App) shutdownGroup(boot *Boot, sel selection, exclude map[string]bool) restoreGroup {
	cutoff := boot.UpdatedAt.Add(-sel.Recent)
	var chosen []Session
	for _, session := range boot.Sessions {
		if !sel.includes(session) || exclude[session.ID] {
			continue
		}
		if sel.All || !session.LastSeen.Before(cutoff) {
			chosen = append(chosen, session)
		}
	}
	return restoreGroup{
		Title:    fmt.Sprintf("Running before the reboot (boot %s, last snapshot %s):", shortID(boot.BootID), fmtTime(boot.UpdatedAt)),
		Sessions: newestPerProcess(chosen),
	}
}

// endedGroup returns the sessions of the current boot that stopped after `after`,
// limited to the most recent batch: the ones last seen within Recent of the latest.
// That is what a terminal quitting or crashing looks like.
func (a *App) endedGroup(boot *Boot, running runningSet, after time.Time, sel selection) restoreGroup {
	if boot == nil {
		return restoreGroup{}
	}
	var ended []Session
	var latest time.Time
	for _, session := range boot.Sessions {
		if !sel.includes(session) || running.ids[session.ID] || running.procs[keyOf(session)] || !session.LastSeen.After(after) {
			continue
		}
		ended = append(ended, session)
		if session.LastSeen.After(latest) {
			latest = session.LastSeen
		}
	}
	chosen := ended
	if !sel.All {
		chosen = nil
		cutoff := latest.Add(-sel.Recent)
		for _, session := range ended {
			if !session.LastSeen.Before(cutoff) {
				chosen = append(chosen, session)
			}
		}
	}
	return restoreGroup{
		Title:    fmt.Sprintf("Ended during this boot, last seen %s:", fmtTime(latest)),
		Sessions: newestPerProcess(chosen),
	}
}

// newestPerProcess keeps one session per Claude Code process: after /clear a
// process moves on to a new session id, and only the latest one is worth reopening.
func newestPerProcess(sessions []Session) []Session {
	newest := map[processKey]Session{}
	for _, session := range sessions {
		key := keyOf(session)
		if best, ok := newest[key]; !ok || seenLater(session, best) {
			newest[key] = session
		}
	}
	chosen := make([]Session, 0, len(newest))
	for _, session := range newest {
		chosen = append(chosen, session)
	}
	sortByStart(chosen)
	return chosen
}

func seenLater(a, b Session) bool {
	if !a.LastSeen.Equal(b.LastSeen) {
		return a.LastSeen.After(b.LastSeen)
	}
	return a.FirstSeen.After(b.FirstSeen)
}

// restoreGroups decides what restore offers. "auto" offers what was lost since the
// last restore: the sessions a reboot took down, until a restore has run in this
// boot, and the latest batch that ended during this boot.
func (a *App) restoreGroups(which string, sel selection, current BootInfo, running runningSet) ([]restoreGroup, error) {
	boots := a.allBoots()
	if len(boots) == 0 {
		return nil, fmt.Errorf("no snapshots in %s yet. The snapshot job has to run before there is anything to restore", a.fmtPath(a.bootsDir()))
	}
	thisBoot := selectBoot(boots, "current", current.ID)
	previous := selectBoot(boots, "previous", current.ID)
	switch which {
	case "auto":
		lastRestore := a.lastRestore(current.ID)
		ended := a.endedGroup(thisBoot, running, lastRestore, sel)
		var groups []restoreGroup
		if previous != nil && lastRestore.IsZero() {
			exclude := map[string]bool{}
			for _, session := range ended.Sessions {
				exclude[session.ID] = true
			}
			groups = appendNonEmpty(groups, a.shutdownGroup(previous, sel, exclude))
		}
		return appendNonEmpty(groups, ended), nil
	case "previous":
		if previous == nil {
			return nil, errors.New("no snapshot from an earlier boot. The snapshot job has to run at least once before a reboot")
		}
		return appendNonEmpty(nil, a.shutdownGroup(previous, sel, nil)), nil
	case "current":
		if thisBoot == nil {
			return nil, errors.New("no snapshot from this boot yet")
		}
		return appendNonEmpty(nil, a.endedGroup(thisBoot, running, time.Time{}, sel)), nil
	default:
		boot := selectBoot(boots, which, current.ID)
		if boot == nil {
			return nil, fmt.Errorf("no snapshot found for boot %q", which)
		}
		if boot.BootID == current.ID {
			return appendNonEmpty(nil, a.endedGroup(boot, running, time.Time{}, sel)), nil
		}
		return appendNonEmpty(nil, a.shutdownGroup(boot, sel, nil)), nil
	}
}

func appendNonEmpty(groups []restoreGroup, group restoreGroup) []restoreGroup {
	if len(group.Sessions) == 0 {
		return groups
	}
	return append(groups, group)
}

func (a *App) restoreStatus(session Session, running runningSet) string {
	switch {
	case !sessionIDPattern.MatchString(session.ID):
		return statusInvalid
	case running.ids[session.ID]:
		return statusRunning
	case !isDir(session.Cwd):
		return statusMissingDirectory
	case !a.transcriptExists(session.ID):
		return statusMissingTranscript
	default:
		return statusReopen
	}
}

func isDir(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

type restoreMarker struct {
	BootID     string    `json:"boot_id"`
	RestoredAt time.Time `json:"restored_at"`
}

func (a *App) markerPath() string {
	return filepath.Join(a.StateDir, "last-restore.json")
}

func (a *App) lastRestore(bootID string) time.Time {
	var marker restoreMarker
	if !readJSON(a.markerPath(), &marker) || marker.BootID != bootID {
		return time.Time{}
	}
	return marker.RestoredAt
}

func (a *App) markRestored(bootID string, at time.Time) error {
	return writePrivate(a.markerPath(), restoreMarker{BootID: bootID, RestoredAt: at})
}
