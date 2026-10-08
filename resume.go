package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

type localSession struct {
	ID      string
	Slug    string
	Project string
	File    string
	ModTime time.Time
}

// findLocalSessions finds transcripts under ~/.claude/projects whose session id
// starts with query, one per session id (the most recently written copy).
func (a *App) findLocalSessions(query string) []localSession {
	byID := map[string]localSession{}
	for _, folder := range a.projectFolders() {
		dir := filepath.Join(a.projectsRoot(), folder.Slug)
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			id, isTranscript := strings.CutSuffix(entry.Name(), ".jsonl")
			if entry.IsDir() || !isTranscript || !strings.HasPrefix(id, query) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			found := localSession{ID: id, Slug: folder.Slug, Project: folder.Path, File: filepath.Join(dir, entry.Name()), ModTime: info.ModTime()}
			if existing, ok := byID[id]; !ok || found.ModTime.After(existing.ModTime) {
				byID[id] = found
			}
		}
	}
	sessions := make([]localSession, 0, len(byID))
	for _, session := range byID {
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return sessions
}

// backupSession is one session found in a backup index: its transcript and every
// file under its own folder (subagent transcripts, tool results).
type backupSession struct {
	ID      string
	Project string
	Entries []fileEntry
}

func findBackupSessions(index backupIndex, query string) []backupSession {
	byID := map[string]*backupSession{}
	slugOf := map[string]string{}
	for _, entry := range index.Files {
		if !isSessionPath(entry.Path) {
			continue
		}
		parts := strings.Split(entry.Path, "/")
		id := strings.TrimSuffix(parts[3], ".jsonl")
		if strings.HasPrefix(id, query) {
			byID[id] = &backupSession{ID: id, Project: entry.Project, Entries: []fileEntry{entry}}
			slugOf[id] = parts[2]
		}
	}
	for _, entry := range index.Files {
		for id, session := range byID {
			if strings.HasPrefix(entry.Path, "claude/projects/"+slugOf[id]+"/"+id+"/") {
				session.Entries = append(session.Entries, entry)
			}
		}
	}
	sessions := make([]backupSession, 0, len(byID))
	for _, session := range byID {
		sessions = append(sessions, *session)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return sessions
}

func (a *App) cmdResume(args []string) int {
	set := flag.NewFlagSet("resume", flag.ContinueOnError)
	from := set.String("from", "", "backup to fetch the session from (git remote or folder); default: your backup destinations")
	identity := set.String("identity", "", "file with the backup's private key, or - for standard input (default: ask)")
	in := set.String("in", "", "folder to resume the session in (default: its project folder)")
	var search stringList
	set.Var(&search, "search", "folder to look for the project's repository in (repeatable; default: your home folder)")
	printOnly := set.Bool("print", false, "print the command instead of running it")
	claudeCommand := set.String("claude-command", a.defaultClaudeCommand(), "command that starts Claude Code, flags allowed")
	positional, code := a.parseInterspersed(set, args)
	if code >= 0 {
		return code
	}
	if len(positional) != 1 {
		fmt.Fprintln(a.Stderr, "Usage: claude-afterlife resume <session-id> [-in <folder>] [-from <git-remote>] [-print]")
		return 2
	}
	query := strings.ToLower(strings.TrimSpace(positional[0]))
	if len(query) < 4 || !sessionIDPattern.MatchString(query+strings.Repeat("0", max(0, 8-len(query)))) {
		return a.fail(errors.New("give at least the first 4 characters of the session id"))
	}
	inFolder := ""
	if *in != "" {
		inFolder = filepath.Clean(a.expandHome(*in))
		if !filepath.IsAbs(inFolder) || !isDir(inFolder) {
			return a.fail(fmt.Errorf("%s is not a folder", *in))
		}
	}
	current, err := a.Boot()
	if err != nil {
		return a.fail(err)
	}
	for _, session := range a.liveSessions(current) {
		if strings.HasPrefix(session.ID, query) {
			return a.fail(fmt.Errorf("session %s is running (process %d, in %s); close it first, two copies would mix up its history", shortID(session.ID), session.PID, a.fmtPath(session.Cwd)))
		}
	}

	var id, folder string
	if *from == "" {
		found := a.findLocalSessions(query)
		if len(found) > 1 {
			return a.fail(ambiguousSessions(found))
		}
		if len(found) == 1 {
			id, folder, err = a.placeLocalSession(found[0], inFolder)
			if err != nil {
				return a.fail(err)
			}
		}
	}
	if id == "" {
		id, folder, err = a.fetchSessionFromBackup(query, *from, *identity, inFolder, search)
		if err != nil {
			return a.fail(err)
		}
	}
	command := resumeCommand(*claudeCommand, id)
	if *printOnly {
		fmt.Fprintf(a.Stdout, "cd %s && %s\n", shellQuote(folder), command)
		return 0
	}
	fmt.Fprintf(a.Stderr, "Resuming %s in %s\n", shortID(id), a.fmtPath(folder))
	if err := a.Exec(folder, []string{"/bin/sh", "-c", command}); err != nil {
		return a.fail(err)
	}
	return 0
}

func ambiguousSessions(found []localSession) error {
	var lines []string
	for _, session := range found {
		lines = append(lines, fmt.Sprintf("  %s  %s", session.ID, session.Project))
	}
	return fmt.Errorf("several sessions start with that; give more of the id:\n%s", strings.Join(lines, "\n"))
}

// placeLocalSession returns where to resume a session found on this machine. With
// -in a different folder, the session is copied there first, because Claude Code
// only finds a session from its own project folder.
func (a *App) placeLocalSession(session localSession, inFolder string) (string, string, error) {
	switch {
	case inFolder == "" && session.Project != "" && isDir(session.Project):
		return session.ID, session.Project, nil
	case inFolder == "" && session.Project == "":
		return "", "", fmt.Errorf("could not tell which folder session %s belongs to; pass -in <folder>", shortID(session.ID))
	case inFolder == "":
		return "", "", fmt.Errorf("the folder of session %s, %s, no longer exists; pass -in <folder>", shortID(session.ID), a.fmtPath(session.Project))
	case inFolder == session.Project:
		return session.ID, inFolder, nil
	}
	mapper := newPathMapper([]projectMapping{{Old: session.Project, New: inFolder}})
	sourceDir := filepath.Join(a.projectsRoot(), session.Slug)
	targetDir := filepath.Join(a.projectsRoot(), encodeProjectPath(inFolder))
	files := []string{session.File}
	sideFiles, _ := a.walkFiles(filepath.Join(sourceDir, session.ID))
	files = append(files, sideFiles...)
	for _, file := range files {
		rel, _ := filepath.Rel(sourceDir, file)
		target := filepath.Join(targetDir, rel)
		if _, err := os.Lstat(target); err == nil {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", "", err
		}
		if filepath.Ext(file) == ".jsonl" && session.Project != "" {
			data = rewriteLines(data, mapper)
		}
		if err := writeFileAtomic(target, data, 0o600); err != nil {
			return "", "", err
		}
	}
	fmt.Fprintf(a.Stderr, "Copied session %s to %s.\n", shortID(session.ID), a.fmtPath(inFolder))
	return session.ID, inFolder, nil
}

func rewriteLines(data []byte, mapper pathMapper) []byte {
	var out []byte
	for len(data) > 0 {
		end := len(data)
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			end = i + 1
		}
		out = append(out, rewriteTopLevelString(data[:end], "cwd", mapper.mapPath)...)
		data = data[end:]
	}
	return out
}

// fetchSessionFromBackup restores one session from a backup and returns where to
// resume it: from -from, or else from a destination whose backup holds it.
func (a *App) fetchSessionFromBackup(query, from, identity, inFolder string, search []string) (string, string, error) {
	notFound := fmt.Errorf("no session %s on this machine; if it is in a backup, pass -from <git-remote>", query)
	var st store
	var cleanup = func() {}
	defer func() { cleanup() }()
	switch {
	case from != "":
		opened, done, err := a.openBackupSource(from)
		if err != nil {
			return "", "", err
		}
		st, cleanup = opened, done
	default:
		config, err := a.loadBackupConfig()
		if err != nil {
			return "", "", err
		}
		name := a.destinationHolding(config, query)
		if name == "" {
			return "", "", notFound
		}
		st = a.storeFor(name)
		if _, err := st.readMeta(); err != nil {
			remote := config.Destinations[name].Remote
			if remote == "" {
				return "", "", fmt.Errorf("the local copy of backup %q is missing and it has no remote", name)
			}
			opened, done, err := a.cloneStore(remote)
			if err != nil {
				return "", "", fmt.Errorf("fetching %s: %w", remote, err)
			}
			st, cleanup = opened, done
		}
	}
	meta, err := st.readMeta()
	if err != nil {
		return "", "", err
	}
	key, err := a.readBackupKey(identity)
	if err != nil {
		return "", "", err
	}
	if !slices.Contains(meta.Recipients, key.recipient) {
		return "", "", errWrongKey
	}
	var index backupIndex
	var session backupSession
	for _, machine := range st.machineIDs() {
		candidate, err := st.readIndex(machine, key)
		if err != nil {
			return "", "", err
		}
		found := findBackupSessions(candidate, query)
		if len(found) > 1 {
			var ids []string
			for _, s := range found {
				ids = append(ids, "  "+s.ID)
			}
			return "", "", fmt.Errorf("several sessions in the backup start with that; give more of the id:\n%s", strings.Join(ids, "\n"))
		}
		if len(found) == 1 && (session.ID == "" || candidate.UpdatedAt.After(index.UpdatedAt)) {
			index, session = candidate, found[0]
		}
	}
	if session.ID == "" {
		return "", "", fmt.Errorf("session %s is not in this backup either", query)
	}

	folder := inFolder
	if folder == "" {
		mappings, err := a.planMappings(backupIndex{Home: index.Home, Projects: index.Projects, Files: session.Entries}, restoreOptions{Search: search})
		if err != nil {
			return "", "", err
		}
		if a.StdinTTY && len(mappings) == 1 && mappings[0].New == "" {
			a.askForFolders(mappings)
		}
		if len(mappings) != 1 || mappings[0].New == "" {
			return "", "", fmt.Errorf("could not find the folder for %s (%s); pass -in <folder>", a.fmtPath(session.Project), describeRepo(projectOf(index, session.Project)))
		}
		folder = mappings[0].New
	}
	mapper := newPathMapper([]projectMapping{{Old: session.Project, New: folder}})
	targetDir := filepath.Join(a.projectsRoot(), encodeProjectPath(folder))
	prefix := strings.TrimSuffix(session.Entries[0].Path, session.ID+".jsonl")
	for _, entry := range session.Entries {
		if err := st.assemble(entry, key.identities, io.Discard); err != nil {
			return "", "", fmt.Errorf("the backup is damaged (%s: %v); nothing was written", displayStorePath(entry.Path), err)
		}
	}
	for _, entry := range session.Entries {
		rel := strings.TrimPrefix(entry.Path, prefix)
		if !filepath.IsLocal(rel) {
			continue
		}
		item := restoreItem{entry: entry, target: filepath.Join(targetDir, filepath.FromSlash(rel))}
		if _, err := a.restoreFile(st, key, item, mapper.movesAnything(), mapper); err != nil {
			return "", "", fmt.Errorf("%s: %w", displayStorePath(entry.Path), err)
		}
	}
	now := a.Now()
	_ = os.Chtimes(filepath.Join(targetDir, session.ID+".jsonl"), now, now)
	fmt.Fprintf(a.Stderr, "Restored session %s from the backup of %s into %s.\n", shortID(session.ID), index.Hostname, a.fmtPath(folder))
	return session.ID, folder, nil
}

func projectOf(index backupIndex, path string) projectInfo {
	info, _ := index.project(path)
	return info
}

// destinationHolding checks the local backup records, which need no key, for a
// destination whose backup holds the session.
func (a *App) destinationHolding(config *backupConfig, query string) string {
	for _, name := range config.destinationNames() {
		var state backupIndex
		if readJSON(a.backupStatePath(name), &state) && len(findBackupSessions(state, query)) > 0 {
			return name
		}
	}
	return ""
}
