package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const restoredCleanupDays = 3650

type restoreOptions struct {
	From        string
	Identity    string
	Machine     string
	Maps        []string
	Search      []string
	DryRun      bool
	Yes         bool
	KeepCleanup bool
}

type projectMapping struct {
	Old      string
	New      string
	Reason   string
	Info     projectInfo
	Sessions int
}

type restoreItem struct {
	entry  fileEntry
	target string
}

type restorePlan struct {
	index        backupIndex
	mappings     []projectMapping
	mapper       pathMapper
	projectFiles []restoreItem
	history      *fileEntry
	settings     []fileEntry
	configFiles  []restoreItem
	boots        []restoreItem
	skippedFiles int
	oldest       time.Time
	cleanupDays  int
}

type restoreResult struct {
	Sessions      int
	Files         int
	Unchanged     int
	Kept          []string
	HistoryLines  int
	SettingsAdded []string
	ConfigWritten int
	ConfigKept    int
	Boots         int
}

func (a *App) openBackupSource(from string) (store, func(), error) {
	path := a.expandHome(from)
	if isDir(path) {
		st := store{dir: path}
		if _, err := st.readMeta(); err == nil {
			return st, func() {}, nil
		}
	}
	st, cleanup, err := a.cloneStore(from)
	if err != nil {
		return store{}, nil, fmt.Errorf("fetching %s: %w", from, err)
	}
	return st, cleanup, nil
}

func (a *App) chooseIndex(st store, key backupKey, machine string) (backupIndex, error) {
	ids := st.machineIDs()
	if len(ids) == 0 {
		return backupIndex{}, errors.New("this backup has no machines in it yet; run backup run on the machine you are backing up")
	}
	var chosen *backupIndex
	for _, id := range ids {
		if machine != "" && !strings.HasPrefix(id, machine) {
			continue
		}
		index, err := st.readIndex(id, key)
		if err != nil {
			return backupIndex{}, err
		}
		if chosen == nil || index.UpdatedAt.After(chosen.UpdatedAt) {
			chosen = &index
		}
	}
	if chosen == nil {
		return backupIndex{}, fmt.Errorf("no machine %q in this backup", machine)
	}
	return *chosen, nil
}

func (a *App) parseMaps(maps []string) ([]projectMapping, error) {
	var parsed []projectMapping
	for _, value := range maps {
		oldPath, newPath, ok := strings.Cut(value, "=")
		oldPath, newPath = filepath.Clean(a.expandHome(oldPath)), filepath.Clean(a.expandHome(newPath))
		if !ok || !filepath.IsAbs(oldPath) || !filepath.IsAbs(newPath) {
			return nil, fmt.Errorf("--map %q: use --map /old/path=/new/path", value)
		}
		parsed = append(parsed, projectMapping{Old: oldPath, New: newPath})
	}
	return parsed, nil
}

// planMappings proposes where each backed-up project's code lives now: an explicit
// --map, the same path, a repository with the same remote, or the same place under
// a moved home folder. Anything else is left for the user.
func (a *App) planMappings(index backupIndex, opts restoreOptions) ([]projectMapping, error) {
	explicit, err := a.parseMaps(opts.Maps)
	if err != nil {
		return nil, err
	}
	explicitMapper := newPathMapper(explicit)
	sessions := map[string]int{}
	for _, entry := range index.Files {
		if entry.Project == "" {
			continue
		}
		if _, ok := sessions[entry.Project]; !ok {
			sessions[entry.Project] = 0
		}
		if isSessionPath(entry.Path) {
			sessions[entry.Project]++
		}
	}
	paths := make([]string, 0, len(sessions))
	for path := range sessions {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	roots := opts.Search
	if len(roots) == 0 {
		roots = []string{a.HomeDir}
	}
	var repos map[string][]string
	var mappings []projectMapping
	for _, old := range paths {
		info, _ := index.project(old)
		mapping := projectMapping{Old: old, Info: info, Sessions: sessions[old]}
		if mapped, ok := explicitMapper.mapPath(old); ok {
			mapping.New, mapping.Reason = mapped, "--map"
		} else if isDir(old) {
			mapping.New, mapping.Reason = old, "same path"
		} else {
			if info.Remote != "" {
				if repos == nil {
					repos = a.findRepos(roots, 4)
				}
				if found := pickRepo(repos[normalizeRemote(info.Remote)], old, info); found != "" {
					mapping.New, mapping.Reason = found, "same git remote"
				}
			}
			if mapping.New == "" && index.Home != "" && index.Home != a.HomeDir && strings.HasPrefix(old, index.Home+"/") {
				if candidate := filepath.Join(a.HomeDir, strings.TrimPrefix(old, index.Home+"/")); isDir(candidate) {
					mapping.New, mapping.Reason = candidate, "home folder moved"
				}
			}
		}
		mappings = append(mappings, mapping)
	}
	return mappings, nil
}

func pickRepo(candidates []string, old string, info projectInfo) string {
	if len(candidates) == 0 {
		return ""
	}
	choice := candidates[0]
	for _, candidate := range candidates {
		if filepath.Base(candidate) == filepath.Base(info.RepoRoot) {
			choice = candidate
			break
		}
	}
	rel := ""
	if info.RepoRoot != "" && strings.HasPrefix(old, info.RepoRoot+"/") {
		rel = strings.TrimPrefix(old, info.RepoRoot+"/")
	}
	if target := filepath.Join(choice, rel); isDir(target) {
		return target
	}
	return choice
}

func describeRepo(info projectInfo) string {
	switch {
	case info.Remote == "":
		return "not a git repository"
	case info.Branch != "":
		return info.Remote + ", branch " + info.Branch
	default:
		return info.Remote
	}
}

func (a *App) askForFolders(mappings []projectMapping) {
	for i := range mappings {
		mapping := &mappings[i]
		if mapping.New != "" {
			continue
		}
		fmt.Fprintf(a.Stdout, "\nNo folder found for %s (%s, %d sessions).\n", a.fmtPath(mapping.Old), describeRepo(mapping.Info), mapping.Sessions)
		for range 3 {
			answer, ok := a.prompt("Folder for it, or Enter to skip: ")
			if !ok || answer == "" {
				break
			}
			path := filepath.Clean(a.expandHome(answer))
			if filepath.IsAbs(path) && isDir(path) {
				mapping.New, mapping.Reason = path, "you chose"
				break
			}
			fmt.Fprintf(a.Stdout, "%s is not a folder.\n", answer)
		}
	}
}

func (a *App) buildRestorePlan(st store, key backupKey, index backupIndex, mappings []projectMapping, opts restoreOptions) (*restorePlan, error) {
	plan := &restorePlan{index: index, mappings: mappings, mapper: newPathMapper(mappings)}
	var restoredSettings []byte
	for _, entry := range index.Files {
		rel, isClaude := strings.CutPrefix(entry.Path, "claude/")
		switch {
		case isClaude && strings.HasPrefix(rel, "projects/"):
			_, inProject, ok := strings.Cut(strings.TrimPrefix(rel, "projects/"), "/")
			if !ok || !filepath.IsLocal(inProject) {
				continue
			}
			newProject, mapped := plan.mapper.mapPath(entry.Project)
			if entry.Project == "" || !mapped {
				plan.skippedFiles++
				continue
			}
			target := filepath.Join(a.projectsRoot(), encodeProjectPath(newProject), filepath.FromSlash(inProject))
			plan.projectFiles = append(plan.projectFiles, restoreItem{entry: entry, target: target})
			if plan.oldest.IsZero() || entry.ModTime.Before(plan.oldest) {
				plan.oldest = entry.ModTime
			}
		case entry.Path == "claude/history.jsonl":
			history := entry
			plan.history = &history
		case entry.Path == "claude/settings.json" || entry.Path == "claude/settings.local.json":
			plan.settings = append(plan.settings, entry)
			if entry.Path == "claude/settings.json" {
				var buffer bytes.Buffer
				if err := st.assemble(entry, key.identities, &buffer); err != nil {
					return nil, fmt.Errorf("settings.json: %w", err)
				}
				restoredSettings = buffer.Bytes()
			}
		case isClaude && filepath.IsLocal(rel):
			plan.configFiles = append(plan.configFiles, restoreItem{entry: entry, target: filepath.Join(a.ClaudeDir, filepath.FromSlash(rel))})
		case strings.HasPrefix(entry.Path, "afterlife/boots/"):
			name := strings.TrimPrefix(entry.Path, "afterlife/boots/")
			if filepath.IsLocal(name) && !strings.Contains(name, "/") {
				plan.boots = append(plan.boots, restoreItem{entry: entry, target: filepath.Join(a.bootsDir(), name)})
			}
		}
	}
	if !opts.KeepCleanup && !plan.oldest.IsZero() {
		needed := int(math.Ceil(a.Now().Sub(plan.oldest).Hours()/24)) + 1
		if needed > a.effectiveCleanupDays(restoredSettings) {
			plan.cleanupDays = max(restoredCleanupDays, needed)
		}
	}
	return plan, nil
}

func (a *App) effectiveCleanupDays(restoredSettings []byte) int {
	local, _ := os.ReadFile(filepath.Join(a.ClaudeDir, "settings.json"))
	if settings, err := decodeSettings(local); err == nil {
		if value, ok := settings["cleanupPeriodDays"]; ok {
			return settingNumber(value)
		}
	}
	if settings, err := decodeSettings(restoredSettings); err == nil {
		if value, ok := settings["cleanupPeriodDays"]; ok {
			return settingNumber(value)
		}
	}
	return 30
}

func (a *App) printRestorePlan(plan *restorePlan) {
	index := plan.index
	fmt.Fprintf(a.Stdout, "Backup of %s (machine %s), last backed up %s.\n\nProjects:\n", index.Hostname, shortID(index.MachineID), fmtTime(index.UpdatedAt))
	for _, mapping := range plan.mappings {
		if mapping.New == "" {
			fmt.Fprintf(a.Stdout, "  %s  skipped: no folder (%s), %d sessions\n", a.fmtPath(mapping.Old), describeRepo(mapping.Info), mapping.Sessions)
			continue
		}
		fmt.Fprintf(a.Stdout, "  %s  ->  %s  (%s), %d sessions\n", a.fmtPath(mapping.Old), a.fmtPath(mapping.New), mapping.Reason, mapping.Sessions)
	}
	var also []string
	if plan.history != nil {
		also = append(also, "prompt history")
	}
	if len(plan.settings) > 0 {
		also = append(also, "settings (merged, yours win)")
	}
	if len(plan.configFiles) > 0 {
		also = append(also, fmt.Sprintf("%d configuration files such as CLAUDE.md and skills (only where missing)", len(plan.configFiles)))
	}
	if len(plan.boots) > 0 {
		also = append(also, fmt.Sprintf("claude-afterlife state (%d boots)", len(plan.boots)))
	}
	if len(also) > 0 {
		fmt.Fprintf(a.Stdout, "Also: %s.\n", strings.Join(also, ", "))
	}
	if plan.cleanupDays > 0 {
		fmt.Fprintf(a.Stdout, "cleanupPeriodDays will be set to %d in ~/.claude/settings.json, so Claude Code keeps sessions back to %s.\n", plan.cleanupDays, plan.oldest.Local().Format("2006-01-02"))
	}
}

// checkPlan rebuilds every file the plan would write and checks it against the
// index, so a damaged backup is found before anything on disk changes.
func (a *App) checkPlan(st store, key backupKey, plan *restorePlan) error {
	entries := slices.Clone(plan.settings)
	if plan.history != nil {
		entries = append(entries, *plan.history)
	}
	for _, items := range [][]restoreItem{plan.projectFiles, plan.configFiles, plan.boots} {
		for _, item := range items {
			entries = append(entries, item.entry)
		}
	}
	for _, entry := range entries {
		if err := st.assemble(entry, key.identities, io.Discard); err != nil {
			return fmt.Errorf("the backup is damaged (%s: %v); nothing was written", displayStorePath(entry.Path), err)
		}
	}
	return nil
}

func (a *App) executeRestore(st store, key backupKey, plan *restorePlan) (restoreResult, error) {
	var result restoreResult
	rewrite := plan.mapper.movesAnything()
	for _, item := range plan.projectFiles {
		outcome, err := a.restoreFile(st, key, item, rewrite, plan.mapper)
		if err != nil {
			return result, fmt.Errorf("%s: %w", displayStorePath(item.entry.Path), err)
		}
		switch outcome {
		case outcomeWritten:
			result.Files++
			if isSessionPath(item.entry.Path) {
				result.Sessions++
			}
		case outcomeUnchanged:
			result.Unchanged++
		case outcomeKept:
			result.Kept = append(result.Kept, a.fmtPath(item.target))
		}
	}
	if plan.history != nil {
		added, err := a.restoreHistory(st, key, *plan.history, plan.mapper)
		if err != nil {
			return result, fmt.Errorf("prompt history: %w", err)
		}
		result.HistoryLines = added
	}
	cleanupApplied := false
	for _, entry := range plan.settings {
		days := 0
		if entry.Path == "claude/settings.json" {
			days, cleanupApplied = plan.cleanupDays, true
		}
		added, err := a.restoreSettings(st, key, &entry, filepath.Join(a.ClaudeDir, strings.TrimPrefix(entry.Path, "claude/")), plan.index.Home, days)
		if err != nil {
			return result, err
		}
		result.SettingsAdded = append(result.SettingsAdded, added...)
	}
	if plan.cleanupDays > 0 && !cleanupApplied {
		if _, err := a.restoreSettings(st, key, nil, filepath.Join(a.ClaudeDir, "settings.json"), plan.index.Home, plan.cleanupDays); err != nil {
			return result, err
		}
	}
	for _, item := range plan.configFiles {
		if _, err := os.Lstat(item.target); err == nil {
			result.ConfigKept++
			continue
		}
		if err := a.writeAssembled(st, key, item, false, nil); err != nil {
			return result, fmt.Errorf("%s: %w", displayStorePath(item.entry.Path), err)
		}
		result.ConfigWritten++
	}
	for _, item := range plan.boots {
		if err := a.restoreBoot(st, key, item, plan.mapper); err != nil {
			return result, fmt.Errorf("%s: %w", displayStorePath(item.entry.Path), err)
		}
		result.Boots++
	}
	return result, nil
}

type restoreOutcome int

const (
	outcomeWritten restoreOutcome = iota
	outcomeUnchanged
	outcomeKept
)

// restoreFile writes one project file. An existing file is only replaced by a
// restored one that extends it; a longer or different local copy is kept.
func (a *App) restoreFile(st store, key backupKey, item restoreItem, rewrite bool, mapper pathMapper) (restoreOutcome, error) {
	transform := rewrite && filepath.Ext(item.target) == ".jsonl"
	if _, err := os.Lstat(item.target); err != nil {
		return outcomeWritten, a.writeAssembled(st, key, item, transform, mapper)
	}
	tmp := item.target + ".afterlife-restore"
	defer os.Remove(tmp)
	staged := restoreItem{entry: item.entry, target: tmp}
	if err := a.writeAssembled(st, key, staged, transform, mapper); err != nil {
		return 0, err
	}
	relation, err := prefixRelation(item.target, tmp)
	if err != nil {
		return 0, err
	}
	switch {
	case relation == 0:
		return outcomeUnchanged, nil
	case relation < 0 && item.entry.Kind == kindAppend:
		if err := os.Rename(tmp, item.target); err != nil {
			return 0, err
		}
		return outcomeWritten, os.Chtimes(item.target, item.entry.ModTime, item.entry.ModTime)
	case relation > 0 && item.entry.Kind == kindAppend:
		return outcomeUnchanged, nil
	default:
		return outcomeKept, nil
	}
}

// prefixRelation compares two files: 0 when equal, -1 when a is a strict prefix of
// b, 1 when b is a strict prefix of a, 2 when they differ.
func prefixRelation(a, b string) (int, error) {
	fa, err := os.Open(a)
	if err != nil {
		return 0, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return 0, err
	}
	defer fb.Close()
	bufA, bufB := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if errA != nil && !errors.Is(errA, io.EOF) && !errors.Is(errA, io.ErrUnexpectedEOF) {
			return 0, errA
		}
		if errB != nil && !errors.Is(errB, io.EOF) && !errors.Is(errB, io.ErrUnexpectedEOF) {
			return 0, errB
		}
		common := min(na, nb)
		if !bytes.Equal(bufA[:common], bufB[:common]) {
			return 2, nil
		}
		switch {
		case na == nb && na < len(bufA):
			return 0, nil
		case na < nb:
			return -1, nil
		case nb < na:
			return 1, nil
		}
	}
}

func (a *App) writeAssembled(st store, key backupKey, item restoreItem, transform bool, mapper pathMapper) error {
	if err := os.MkdirAll(filepath.Dir(item.target), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(item.target), ".restore-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	writer := bufio.NewWriterSize(tmp, 1<<20)
	if transform {
		reader, pipe := io.Pipe()
		go func() { pipe.CloseWithError(st.assemble(item.entry, key.identities, pipe)) }()
		lines := bufio.NewReaderSize(reader, 1<<20)
		for {
			line, err := lines.ReadBytes('\n')
			if len(line) > 0 {
				if _, werr := writer.Write(rewriteTopLevelString(line, "cwd", mapper.mapPath)); werr != nil {
					reader.CloseWithError(werr)
					tmp.Close()
					return werr
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				tmp.Close()
				return err
			}
		}
	} else if err := st.assemble(item.entry, key.identities, writer); err != nil {
		tmp.Close()
		return err
	}
	if err := writer.Flush(); err != nil {
		tmp.Close()
		return err
	}
	mode := os.FileMode(item.entry.Mode).Perm()
	if mode == 0 {
		mode = 0o600
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), item.target); err != nil {
		return err
	}
	return os.Chtimes(item.target, item.entry.ModTime, item.entry.ModTime)
}

func (a *App) assembleBytes(st store, key backupKey, entry fileEntry) ([]byte, error) {
	var buffer bytes.Buffer
	err := st.assemble(entry, key.identities, &buffer)
	return buffer.Bytes(), err
}

func (a *App) restoreHistory(st store, key backupKey, entry fileEntry, mapper pathMapper) (int, error) {
	restored, err := a.assembleBytes(st, key, entry)
	if err != nil {
		return 0, err
	}
	path := filepath.Join(a.ClaudeDir, "history.jsonl")
	local, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	merged, added := mergeHistory(local, restored, mapper)
	if added == 0 {
		return 0, nil
	}
	return added, writeFileAtomic(path, merged, 0o600)
}

func (a *App) restoreSettings(st store, key backupKey, entry *fileEntry, path, oldHome string, cleanupDays int) ([]string, error) {
	var restored []byte
	if entry != nil {
		data, err := a.assembleBytes(st, key, *entry)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", displayStorePath(entry.Path), err)
		}
		restored = data
	}
	local, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if errors.Is(err, os.ErrNotExist) && restored != nil && oldHome == a.HomeDir && cleanupDays == 0 {
		settings, err := decodeSettings(restored)
		if err != nil {
			return nil, fmt.Errorf("the backed-up %s is not valid JSON: %w", filepath.Base(path), err)
		}
		added := make([]string, 0, len(settings))
		for key := range settings {
			added = append(added, key)
		}
		sort.Strings(added)
		return added, writeFileAtomic(path, restored, os.FileMode(entry.Mode).Perm()|0o600)
	}
	merged, added, changed, err := mergeSettings(local, restored, oldHome, a.HomeDir, cleanupDays)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.fmtPath(path), err)
	}
	if !changed {
		return nil, nil
	}
	return added, writeFileAtomic(path, merged, 0o600)
}

func (a *App) restoreBoot(st store, key backupKey, item restoreItem, mapper pathMapper) error {
	data, err := a.assembleBytes(st, key, item.entry)
	if err != nil {
		return err
	}
	var restored Boot
	if err := json.Unmarshal(data, &restored); err != nil || restored.Format != storeFormat {
		return errors.New("not a claude-afterlife snapshot")
	}
	if restored.Sessions == nil {
		restored.Sessions = map[string]Session{}
	}
	local, _ := loadBoot(item.target)
	merged := mergeBoot(local, restored, mapper)
	return writePrivate(item.target, merged)
}
