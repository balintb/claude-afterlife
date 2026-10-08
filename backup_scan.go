package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// sourceFile is one thing to back up: a file under ~/.claude, or content generated
// for a destination (prompt history and claude-afterlife state filtered by route).
type sourceFile struct {
	storePath string
	localPath string
	content   []byte
	kind      string
	project   string
}

type projectFolder struct {
	Slug string
	Path string
}

var (
	configFileNames = []string{"settings.json", "settings.local.json", "CLAUDE.md", "keybindings.json"}
	configDirNames  = []string{"skills", "agents", "commands", "hooks", "output-styles"}
	includePattern  = regexp.MustCompile(`(?m)^@(\S+)\s*$`)
)

// encodeProjectPath mirrors how Claude Code names project folders: every character
// that is not an ASCII letter or digit becomes "-", counted in UTF-16 code units.
func encodeProjectPath(path string) string {
	var b strings.Builder
	for _, r := range path {
		switch {
		case r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (a *App) projectsRoot() string {
	return filepath.Join(a.ClaudeDir, "projects")
}

// projectFolders lists ~/.claude/projects with the real path of each folder. The
// folder name cannot be decoded, so the path comes from the cwd recorded in its
// transcripts, Claude Code's own project list, or claude-afterlife's snapshots.
func (a *App) projectFolders() []projectFolder {
	entries, _ := os.ReadDir(a.projectsRoot())
	known := a.knownProjectPaths()
	var folders []projectFolder
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slug := entry.Name()
		path := cwdFromTranscripts(filepath.Join(a.projectsRoot(), slug), slug)
		if path == "" {
			path = known[slug]
		}
		if path == "" {
			path = resolveSlugByWalk("/", slug)
		}
		folders = append(folders, projectFolder{Slug: slug, Path: path})
	}
	return folders
}

func (a *App) knownProjectPaths() map[string]string {
	known := map[string]string{}
	add := func(path string) {
		if filepath.IsAbs(path) {
			if _, taken := known[encodeProjectPath(path)]; !taken {
				known[encodeProjectPath(path)] = path
			}
		}
	}
	for _, configPath := range []string{filepath.Join(a.ClaudeDir, ".claude.json"), filepath.Join(a.HomeDir, ".claude.json")} {
		var config struct {
			Projects map[string]json.RawMessage `json:"projects"`
		}
		if readJSON(configPath, &config) {
			for path := range config.Projects {
				add(path)
			}
		}
	}
	for _, boot := range a.allBoots() {
		for _, session := range boot.Sessions {
			add(session.Cwd)
		}
	}
	return known
}

// resolveSlugByWalk finds the existing folder whose encoding is slug, descending only
// into folders whose encoding is a prefix of it. Several matches (a_b and a-b both
// encode to a-b) count as no match.
func resolveSlugByWalk(root, slug string) string {
	var matches []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 32 || len(matches) > 1 {
			return
		}
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if !entry.IsDir() && !(entry.Type()&os.ModeSymlink != 0 && isDir(path)) {
				continue
			}
			encoded := encodeProjectPath(path)
			switch {
			case encoded == slug:
				matches = append(matches, path)
			case strings.HasPrefix(slug, encoded+"-"):
				walk(path, depth+1)
			}
		}
	}
	walk(root, 0)
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func cwdFromTranscripts(dir, slug string) string {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		if path := firstMatchingCWD(filepath.Join(dir, entry.Name()), slug); path != "" {
			return path
		}
	}
	return ""
}

func firstMatchingCWD(path, slug string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	reader := bufio.NewReaderSize(io.LimitReader(file, 8<<20), 64<<10)
	for range 200 {
		line, err := reader.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"cwd"`)) {
			var fields struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &fields) == nil && fields.Cwd != "" && encodeProjectPath(fields.Cwd) == slug {
				return fields.Cwd
			}
		}
		if err != nil {
			return ""
		}
	}
	return ""
}

func claudeStorePath(rel string) string {
	return "claude/" + filepath.ToSlash(rel)
}

// backupSources lists what goes to one destination. Nothing is included unless a
// route sends it there.
func (a *App) backupSources(config *backupConfig, dest string) ([]sourceFile, error) {
	var sources []sourceFile
	for _, folder := range a.projectFolders() {
		if folder.Path == "" || a.destinationFor(config, folder.Path) != dest {
			continue
		}
		files, err := a.walkFiles(filepath.Join(a.projectsRoot(), folder.Slug))
		if err != nil {
			return nil, err
		}
		for _, path := range files {
			rel, _ := filepath.Rel(a.ClaudeDir, path)
			kind := kindWhole
			if filepath.Ext(path) == ".jsonl" {
				kind = kindAppend
			}
			sources = append(sources, sourceFile{storePath: claudeStorePath(rel), localPath: path, kind: kind, project: folder.Path})
		}
	}
	if history, ok := a.historyFor(config, dest); ok {
		sources = append(sources, sourceFile{storePath: "claude/history.jsonl", content: history, kind: kindWhole})
	}
	sources = append(sources, a.bootsFor(config, dest)...)
	if config.ConfigDestination == dest {
		sources = append(sources, a.configSources()...)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].storePath < sources[j].storePath })
	return sources, nil
}

func (a *App) walkFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// historyFor keeps the prompt-history lines of projects routed to dest; lines with
// no project go with Claude Code's configuration.
func (a *App) historyFor(config *backupConfig, dest string) ([]byte, bool) {
	data, err := os.ReadFile(filepath.Join(a.ClaudeDir, "history.jsonl"))
	if err != nil {
		return nil, false
	}
	var kept bytes.Buffer
	for _, line := range completeLines(data) {
		var fields struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(line, &fields)
		target := config.ConfigDestination
		if fields.Project != "" {
			target = a.destinationFor(config, fields.Project)
		}
		if target == dest {
			kept.Write(line)
		}
	}
	return kept.Bytes(), kept.Len() > 0
}

// bootsFor keeps, from every claude-afterlife snapshot, the sessions of projects
// routed to dest, so the new machine knows which of them were open.
func (a *App) bootsFor(config *backupConfig, dest string) []sourceFile {
	var sources []sourceFile
	for _, boot := range a.allBoots() {
		filtered := *boot
		filtered.Sessions = map[string]Session{}
		for id, session := range boot.Sessions {
			if a.destinationFor(config, session.Cwd) == dest {
				filtered.Sessions[id] = session
			}
		}
		if len(filtered.Sessions) == 0 {
			continue
		}
		filtered.Running = nil
		for _, id := range boot.Running {
			if _, kept := filtered.Sessions[id]; kept {
				filtered.Running = append(filtered.Running, id)
			}
		}
		data, err := json.MarshalIndent(filtered, "", "  ")
		if err != nil {
			continue
		}
		sources = append(sources, sourceFile{
			storePath: "afterlife/boots/" + filepath.Base(a.bootPath(boot.BootID)),
			content:   append(data, '\n'),
			kind:      kindWhole,
		})
	}
	return sources
}

func (a *App) configSources() []sourceFile {
	seen := map[string]bool{}
	var sources []sourceFile
	add := func(path string) {
		rel, err := filepath.Rel(a.ClaudeDir, path)
		if err != nil || !filepath.IsLocal(rel) || seen[rel] {
			return
		}
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			return
		}
		seen[rel] = true
		sources = append(sources, sourceFile{storePath: claudeStorePath(rel), localPath: path, kind: kindWhole})
	}
	for _, name := range configFileNames {
		add(filepath.Join(a.ClaudeDir, name))
	}
	if data, err := os.ReadFile(filepath.Join(a.ClaudeDir, "CLAUDE.md")); err == nil {
		for _, match := range includePattern.FindAllSubmatch(data, -1) {
			include := a.expandHome(string(match[1]))
			if !filepath.IsAbs(include) {
				include = filepath.Join(a.ClaudeDir, include)
			}
			add(filepath.Clean(include))
		}
	}
	for _, dir := range configDirNames {
		files, _ := a.walkFiles(filepath.Join(a.ClaudeDir, dir))
		for _, path := range files {
			add(path)
		}
	}
	return sources
}

func completeLines(data []byte) [][]byte {
	var lines [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, data[:i+1])
		data = data[i+1:]
	}
	return lines
}
