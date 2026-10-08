package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"filippo.io/age"
)

type runReport struct {
	Destination string
	Sessions    int
	Projects    int
	NewFiles    int
	Updated     int
	Rewritten   []string
	Archived    int
	NewBytes    int64
	Objects     int
	Reset       bool
	Committed   bool
	Pushed      bool
	PushError   string
	Dirty       []string
	Unpushed    []string
}

type destinationKeys struct {
	recipients []age.Recipient
	macKey     []byte
}

func (a *App) destinationKeys(config *backupConfig, name string) (store, destinationKeys, error) {
	dest, ok := config.Destinations[name]
	if !ok {
		return store{}, destinationKeys{}, fmt.Errorf("no backup destination named %q", name)
	}
	st := a.storeFor(name)
	meta, err := st.readMeta()
	if err != nil {
		return st, destinationKeys{}, err
	}
	if !slices.Contains(meta.Recipients, dest.Recipient) {
		return st, destinationKeys{}, fmt.Errorf("the store of %q does not match its configuration", name)
	}
	recipients, err := parseRecipients(meta.Recipients)
	if err != nil {
		return st, destinationKeys{}, err
	}
	macKey, err := hex.DecodeString(dest.MACKey)
	if err != nil || len(macKey) != 32 {
		return st, destinationKeys{}, fmt.Errorf("the configuration of %q has an invalid integrity key", name)
	}
	return st, destinationKeys{recipients: recipients, macKey: macKey}, nil
}

func (a *App) loadBackupState(config *backupConfig, name string, st store) (backupIndex, bool) {
	var state backupIndex
	fresh := backupIndex{Format: backupFormat, MachineID: config.MachineID, CreatedAt: a.Now()}
	if !readJSON(a.backupStatePath(name), &state) || state.Format != backupFormat || state.MachineID != config.MachineID {
		return fresh, false
	}
	for _, entry := range state.Files {
		for _, segment := range entry.Segments {
			if !st.hasObject(segment.Object) {
				return fresh, true
			}
		}
	}
	return state, false
}

func (a *App) runBackup(config *backupConfig, name string) (runReport, error) {
	report := runReport{Destination: name}
	st, keys, err := a.destinationKeys(config, name)
	if err != nil {
		return report, err
	}
	state, reset := a.loadBackupState(config, name, st)
	report.Reset = reset
	before := indexFingerprint(state)
	sources, err := a.backupSources(config, name)
	if err != nil {
		return report, err
	}

	entries := map[string]fileEntry{}
	for _, entry := range state.Files {
		entries[entry.Path] = entry
	}
	seen := map[string]bool{}
	projects := map[string]bool{}
	for _, source := range sources {
		seen[source.storePath] = true
		if source.project != "" {
			projects[source.project] = true
		}
		previous, existed := entries[source.storePath]
		var old *fileEntry
		if existed {
			old = &previous
		}
		var entry fileEntry
		var change backupChange
		if source.kind == kindAppend {
			entry, change, err = a.backupAppend(st, keys.recipients, source, old)
		} else {
			entry, change, err = a.backupWhole(st, keys.recipients, source, old)
		}
		if err != nil {
			return report, fmt.Errorf("backing up %s: %w", source.storePath, err)
		}
		entries[source.storePath] = entry
		report.NewBytes += change.bytes
		report.Objects += change.objects
		switch {
		case !existed:
			report.NewFiles++
		case change.rewritten:
			report.Rewritten = append(report.Rewritten, displayStorePath(source.storePath))
		case change.bytes > 0 || change.objects > 0:
			report.Updated++
		}
	}
	for storePath, entry := range entries {
		if !seen[storePath] && !entry.Deleted {
			entry.Deleted = true
			entries[storePath] = entry
		}
	}

	state.Files = state.Files[:0]
	for _, entry := range entries {
		state.Files = append(state.Files, entry)
		if entry.Deleted {
			report.Archived++
		}
		if isSessionPath(entry.Path) {
			report.Sessions++
		}
	}
	sort.Slice(state.Files, func(i, j int) bool { return state.Files[i].Path < state.Files[j].Path })
	a.refreshProjects(&state, projects, &report)
	report.Projects = len(state.Projects)

	hostname, _ := os.Hostname()
	state.Home = a.HomeDir
	indexPath, _, _ := st.indexPaths(config.MachineID)
	_, statErr := os.Stat(indexPath)
	if reset || statErr != nil || indexFingerprint(state) != before {
		state.Hostname = hostname
		state.ClaudeVersion = a.claudeVersion()
		state.UpdatedAt = a.Now()
		if err := st.writeIndex(state, keys.recipients, keys.macKey); err != nil {
			return report, err
		}
		if err := writePrivate(a.backupStatePath(name), state); err != nil {
			return report, err
		}
	}
	report.Committed, err = a.commitStore(st, fmt.Sprintf("backup from %s at %s", hostname, a.Now().UTC().Format("2006-01-02 15:04:05")))
	if err != nil {
		return report, fmt.Errorf("committing the backup: %w", err)
	}
	if config.Destinations[name].Remote != "" {
		if err := a.pushStore(st); err != nil {
			report.PushError = err.Error()
		} else {
			report.Pushed = true
		}
	}
	return report, nil
}

// indexFingerprint covers what a backup records, so an unchanged run writes and
// commits nothing.
func indexFingerprint(index backupIndex) string {
	data, _ := json.Marshal(struct {
		Files    []fileEntry
		Projects []projectInfo
		Home     string
	}{index.Files, index.Projects, index.Home})
	return sha256Hex(data)
}

type backupChange struct {
	bytes     int64
	objects   int
	rewritten bool
}

// backupAppend uploads the complete lines added to a growing file since the last
// backup. When the already-uploaded part no longer matches, the file was rewritten
// rather than appended to, and it is uploaded again from the start.
func (a *App) backupAppend(st store, recipients []age.Recipient, source sourceFile, old *fileEntry) (fileEntry, backupChange, error) {
	var change backupChange
	file, err := os.Open(source.localPath)
	if err != nil {
		return fileEntry{}, change, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileEntry{}, change, err
	}
	size := info.Size()
	running := sha256.New()
	var prefix int64
	var segments []segmentRef
	if old != nil {
		if old.Size <= size {
			if _, err := io.Copy(running, io.NewSectionReader(file, 0, old.Size)); err != nil {
				return fileEntry{}, change, err
			}
			if hex.EncodeToString(running.Sum(nil)) == old.SHA256 {
				prefix, segments = old.Size, slices.Clone(old.Segments)
			}
		}
		if prefix != old.Size || old.Size > size {
			change.rewritten = true
			running.Reset()
			prefix, segments = 0, nil
		}
	}
	end, err := lastLineEnd(file, prefix, size)
	if err != nil {
		return fileEntry{}, change, err
	}
	if end > prefix {
		refs, err := st.putRange(file, prefix, end, recipients, running)
		if err != nil {
			return fileEntry{}, change, err
		}
		segments = append(segments, refs...)
		change.bytes, change.objects = end-prefix, len(refs)
	}
	return fileEntry{
		Path:     source.storePath,
		Kind:     kindAppend,
		Project:  source.project,
		Size:     end,
		SHA256:   hex.EncodeToString(running.Sum(nil)),
		ModTime:  info.ModTime(),
		Mode:     uint32(info.Mode().Perm()),
		Segments: segments,
	}, change, nil
}

func (a *App) backupWhole(st store, recipients []age.Recipient, source sourceFile, old *fileEntry) (fileEntry, backupChange, error) {
	var change backupChange
	data := source.content
	modTime := a.Now()
	mode := uint32(0o600)
	if source.localPath != "" {
		info, err := os.Stat(source.localPath)
		if err != nil {
			return fileEntry{}, change, err
		}
		if data, err = os.ReadFile(source.localPath); err != nil {
			return fileEntry{}, change, err
		}
		modTime, mode = info.ModTime(), uint32(info.Mode().Perm())
	}
	sum := sha256Hex(data)
	if old != nil && old.SHA256 == sum && old.Size == int64(len(data)) {
		unchanged := *old
		unchanged.Deleted = false
		unchanged.Project = source.project
		unchanged.Mode = mode
		return unchanged, change, nil
	}
	refs, err := st.putRange(bytes.NewReader(data), 0, int64(len(data)), recipients, nil)
	if err != nil {
		return fileEntry{}, change, err
	}
	change.bytes, change.objects = int64(len(data)), len(refs)
	return fileEntry{
		Path:     source.storePath,
		Kind:     kindWhole,
		Project:  source.project,
		Size:     int64(len(data)),
		SHA256:   sum,
		ModTime:  modTime,
		Mode:     mode,
		Segments: refs,
	}, change, nil
}

// lastLineEnd returns the position just after the last newline in [from, size), or
// from when there is none: a line Claude Code is still writing waits for the next
// backup.
func lastLineEnd(file io.ReaderAt, from, size int64) (int64, error) {
	buffer := make([]byte, 64<<10)
	for end := size; end > from; {
		start := max(from, end-int64(len(buffer)))
		chunk := buffer[:end-start]
		if _, err := file.ReadAt(chunk, start); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return from, nil
}

func (a *App) refreshProjects(state *backupIndex, current map[string]bool, report *runReport) {
	byPath := map[string]projectInfo{}
	for _, info := range state.Projects {
		byPath[info.Path] = info
	}
	reportedRepos := map[string]bool{}
	for projectPath := range current {
		info := a.repoInfo(projectPath)
		if info.RepoRoot == "" {
			if previous, ok := byPath[projectPath]; ok && previous.RepoRoot != "" {
				continue
			}
		}
		byPath[projectPath] = info
		if info.RepoRoot == "" || reportedRepos[info.RepoRoot] {
			continue
		}
		reportedRepos[info.RepoRoot] = true
		if info.Dirty {
			report.Dirty = append(report.Dirty, info.RepoRoot)
		}
		for _, branch := range a.unpushedBranches(info.RepoRoot) {
			report.Unpushed = append(report.Unpushed, info.RepoRoot+": "+branch)
		}
	}
	state.Projects = state.Projects[:0]
	for _, info := range byPath {
		state.Projects = append(state.Projects, info)
	}
	sort.Slice(state.Projects, func(i, j int) bool { return state.Projects[i].Path < state.Projects[j].Path })
	sort.Strings(report.Dirty)
	sort.Strings(report.Unpushed)
}

func (a *App) claudeVersion() string {
	newest := ""
	dir := filepath.Join(a.ClaudeDir, "sessions")
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		var file claudeSessionFile
		if readJSON(filepath.Join(dir, entry.Name()), &file) && compareVersions(file.Version, newest) > 0 {
			newest = file.Version
		}
	}
	return newest
}

func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &x)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &y)
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// isSessionPath reports whether a store path is a top-level session transcript,
// claude/projects/<folder>/<session>.jsonl, as opposed to a side-transcript.
func isSessionPath(storePath string) bool {
	parts := strings.Split(storePath, "/")
	return len(parts) == 4 && parts[0] == "claude" && parts[1] == "projects" && path.Ext(parts[3]) == ".jsonl"
}

func displayStorePath(storePath string) string {
	return strings.TrimPrefix(strings.TrimPrefix(storePath, "claude/"), "afterlife/")
}
