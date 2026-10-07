package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,127}$`)

// claudeSessionFile is the part of ~/.claude/sessions/<pid>.json this tool reads.
// The format is internal to Claude Code and undocumented.
type claudeSessionFile struct {
	PID       int     `json:"pid"`
	SessionID string  `json:"sessionId"`
	Cwd       string  `json:"cwd"`
	StartedAt float64 `json:"startedAt"`
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	Version   string  `json:"version"`
}

func (f claudeSessionFile) session() (Session, bool) {
	if !sessionIDPattern.MatchString(f.SessionID) || f.PID <= 0 || !filepath.IsAbs(f.Cwd) {
		return Session{}, false
	}
	session := Session{
		ID:            f.SessionID,
		PID:           f.PID,
		Cwd:           f.Cwd,
		Name:          f.Name,
		Kind:          f.Kind,
		ClaudeVersion: f.Version,
	}
	if f.StartedAt > 0 {
		session.StartedAt = time.UnixMilli(int64(f.StartedAt))
	}
	return session, true
}

// readJSON retries briefly because Claude Code rewrites its session files while running.
func readJSON(path string, into any) bool {
	for attempt := range 3 {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		if json.Unmarshal(data, into) == nil {
			return true
		}
		if attempt < 2 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return false
}

func (a *App) liveSessions(boot BootInfo) []Session {
	dir := filepath.Join(a.ClaudeDir, "sessions")
	entries, _ := os.ReadDir(dir)
	seen := map[string]bool{}
	var live []Session
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var file claudeSessionFile
		if !readJSON(filepath.Join(dir, entry.Name()), &file) {
			continue
		}
		session, ok := file.session()
		if !ok || seen[session.ID] {
			continue
		}
		if !boot.Time.IsZero() && !session.StartedAt.IsZero() && session.StartedAt.Before(boot.Time) {
			continue
		}
		if !a.Alive(session.PID) {
			continue
		}
		seen[session.ID] = true
		live = append(live, session)
	}
	sortByStart(live)
	return live
}

func (a *App) transcriptExists(sessionID string) bool {
	projects := filepath.Join(a.ClaudeDir, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(projects, entry.Name(), sessionID+".jsonl")); err == nil {
			return true
		}
	}
	return false
}

func sortByStart(sessions []Session) {
	sort.SliceStable(sessions, func(i, j int) bool {
		if !sessions[i].StartedAt.Equal(sessions[j].StartedAt) {
			return sessions[i].StartedAt.Before(sessions[j].StartedAt)
		}
		return sessions[i].ID < sessions[j].ID
	})
}
