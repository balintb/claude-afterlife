package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var privateKeyPattern = regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Z]+`)

type machine struct {
	t      *testing.T
	home   string
	app    *App
	stdout bytes.Buffer
	stderr bytes.Buffer
	now    time.Time
	boot   BootInfo
	alive  map[int]bool
}

// isolateGit keeps the developer's git configuration (signing, hooks, identity)
// out of the tests.
func isolateGit(t *testing.T) {
	t.Helper()
	config := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(config, []byte("[init]\n\tdefaultBranch = main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "Test")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "test@example.com")
	}
}

func newMachine(t *testing.T, home string, bootID string) *machine {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &machine{
		t:     t,
		home:  home,
		now:   time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
		boot:  BootInfo{ID: bootID, Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		alive: map[int]bool{},
	}
	m.app = &App{
		HomeDir:   home,
		ClaudeDir: filepath.Join(home, ".claude"),
		StateDir:  filepath.Join(home, ".local", "state", "claude-afterlife"),
		KeepBoots: 20,
		GOOS:      "darwin",
		Stdin:     strings.NewReader(""),
		Stdout:    &m.stdout,
		Stderr:    &m.stderr,
		Getenv:    func(key string) string { return map[string]string{"SHELL": "/bin/zsh"}[key] },
		Now:       func() time.Time { return m.now },
		Boot:      func() (BootInfo, error) { return m.boot, nil },
		Alive:     func(pid int) bool { return m.alive[pid] },
		LookPath:  func(string) (string, error) { return "", errors.New("no terminals in tests") },
		Run: func(argv []string) (string, error) {
			t.Fatalf("unexpected command %q", argv)
			return "", nil
		},
		Executable: func() (string, error) { return "/usr/local/bin/claude-afterlife", nil },
		Git:        runGit,
		ReadSecret: func(string) (string, error) { return "", errors.New("no terminal in tests") },
	}
	return m
}

func (m *machine) run(args ...string) int {
	m.stdout.Reset()
	m.stderr.Reset()
	m.app.lines = nil
	return m.app.Main(args)
}

func (m *machine) mustRun(args ...string) string {
	m.t.Helper()
	if code := m.run(args...); code != 0 {
		m.t.Fatalf("%v exited %d\nstdout:\n%s\nstderr:\n%s", args, code, m.stdout.String(), m.stderr.String())
	}
	return m.stdout.String()
}

func (m *machine) withInput(tty bool, input string) {
	m.app.StdinTTY = tty
	m.app.Stdin = strings.NewReader(input)
	m.app.lines = nil
}

func (m *machine) path(rel string) string {
	return filepath.Join(m.home, filepath.FromSlash(rel))
}

func (m *machine) write(path, content string, mode os.FileMode, modTime time.Time) {
	m.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		m.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		m.t.Fatal(err)
	}
	if !modTime.IsZero() {
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			m.t.Fatal(err)
		}
	}
}

func (m *machine) appendTo(path, content string) {
	m.t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		m.t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(content); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) gitRepo(dir, remote string) {
	m.t.Helper()
	m.write(filepath.Join(dir, "README.md"), "# "+filepath.Base(dir)+"\n", 0o644, time.Time{})
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "initial"}, {"remote", "add", "origin", remote}} {
		if _, err := runGit(dir, args...); err != nil {
			m.t.Fatalf("git %v in %s: %v", args, dir, err)
		}
	}
}

func transcriptLine(cwd, session string, n int, extra string) string {
	line, _ := json.Marshal(map[string]any{
		"type":      "user",
		"cwd":       cwd,
		"sessionId": session,
		"uuid":      fmt.Sprintf("%s-%04d", session, n),
		"message":   map[string]any{"role": "user", "content": fmt.Sprintf("message %d in %s: %s", n, filepath.Base(cwd), extra), "cwd": "/nested/is/left/alone"},
	})
	return string(line) + "\n"
}

func transcript(cwd, session string, from, to int, extra string) string {
	var b strings.Builder
	for n := from; n < to; n++ {
		b.WriteString(transcriptLine(cwd, session, n, extra))
	}
	return b.String()
}

func (m *machine) sessionPath(project, session string) string {
	return filepath.Join(m.app.projectsRoot(), encodeProjectPath(project), session+".jsonl")
}

func (m *machine) addSession(project, session string, lines int, modTime time.Time) string {
	path := m.sessionPath(project, session)
	m.write(path, transcript(project, session, 0, lines, `quote " backslash \ unicode é`), 0o600, modTime)
	return path
}

// initDestination creates a destination with a bare repository as its remote and
// returns the path of a file holding the private key, as a password manager would.
func (m *machine) initDestination(name, remote string) string {
	m.t.Helper()
	if _, err := runGit(filepath.Dir(remote), "init", "-q", "--bare", filepath.Base(remote)); err != nil {
		m.t.Fatal(err)
	}
	out := m.mustRun("backup", "init", name, remote, "-confirm-key-saved")
	secret := privateKeyPattern.FindString(out)
	if secret == "" {
		m.t.Fatalf("no private key in:\n%s", out)
	}
	keyFile := filepath.Join(filepath.Dir(remote), name+".key")
	m.write(keyFile, secret+"\n", 0o600, time.Time{})
	return keyFile
}

// snapshot reads every file under root, relative path -> content.
func snapshot(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		rel, _ := filepath.Rel(root, path)
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	return files
}

func (m *machine) openStore(name, keyFile string) (store, backupKey, backupIndex) {
	m.t.Helper()
	data, err := os.ReadFile(keyFile)
	if err != nil {
		m.t.Fatal(err)
	}
	key, err := parseBackupKey(string(data))
	if err != nil {
		m.t.Fatal(err)
	}
	config, err := m.app.loadBackupConfig()
	if err != nil {
		m.t.Fatal(err)
	}
	st := m.app.storeFor(name)
	index, err := st.readIndex(config.MachineID, key)
	if err != nil {
		m.t.Fatal(err)
	}
	return st, key, index
}

func entryFor(t *testing.T, index backupIndex, suffix string) fileEntry {
	t.Helper()
	for _, entry := range index.Files {
		if strings.HasSuffix(entry.Path, suffix) {
			return entry
		}
	}
	t.Fatalf("no entry ending in %s among %d files", suffix, len(index.Files))
	return fileEntry{}
}
