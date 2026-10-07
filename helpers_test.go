package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	id1 = "11111111-1111-4111-8111-111111111111"
	id2 = "22222222-2222-4222-8222-222222222222"
	id3 = "33333333-3333-4333-8333-333333333333"
)

type fixture struct {
	t      *testing.T
	app    *App
	root   string
	work   string
	env    map[string]string
	alive  map[int]bool
	boot   BootInfo
	now    time.Time
	paths  map[string]string
	calls  [][]string
	runOut string
	runErr error
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func at(seconds float64) time.Time {
	return time.UnixMilli(int64(seconds * 1000))
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:      t,
		root:   root,
		work:   filepath.Join(root, "work"),
		env:    map[string]string{"SHELL": "/bin/zsh"},
		alive:  map[int]bool{},
		boot:   BootInfo{ID: "boot-a", Time: at(1000)},
		now:    at(3000),
		paths:  map[string]string{},
		runOut: "42",
	}
	for _, dir := range []string{f.work, filepath.Join(root, "claude", "sessions")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.app = &App{
		ClaudeDir: filepath.Join(root, "claude"),
		StateDir:  filepath.Join(root, "state"),
		HomeDir:   root,
		KeepBoots: 20,
		GOOS:      "darwin",
		Stdin:     strings.NewReader(""),
		Stdout:    &f.stdout,
		Stderr:    &f.stderr,
		Getenv:    func(key string) string { return f.env[key] },
		Now:       func() time.Time { return f.now },
		Boot:      func() (BootInfo, error) { return f.boot, nil },
		Alive:     func(pid int) bool { return f.alive[pid] },
		LookPath: func(file string) (string, error) {
			if path, ok := f.paths[file]; ok {
				return path, nil
			}
			return "", errors.New("not found")
		},
		Run: func(argv []string) (string, error) {
			f.calls = append(f.calls, argv)
			return f.runOut, f.runErr
		},
		Executable: func() (string, error) { return "/usr/local/bin/claude-afterlife", nil },
	}
	return f
}

type sessionOption func(*claudeSessionFile, *bool)

func withCwd(cwd string) sessionOption {
	return func(file *claudeSessionFile, _ *bool) { file.Cwd = cwd }
}

func withKind(kind string) sessionOption {
	return func(file *claudeSessionFile, _ *bool) { file.Kind = kind }
}

func withoutTranscript() sessionOption {
	return func(_ *claudeSessionFile, transcript *bool) { *transcript = false }
}

func (f *fixture) addSession(id string, pid int, started float64, options ...sessionOption) {
	f.t.Helper()
	file := claudeSessionFile{
		PID:       pid,
		SessionID: id,
		Cwd:       f.work,
		StartedAt: started * 1000,
		Kind:      "interactive",
		Name:      "session-" + id[:4],
		Version:   "2.1.0",
	}
	transcript := true
	for _, option := range options {
		option(&file, &transcript)
	}
	f.writeJSON(filepath.Join(f.app.ClaudeDir, "sessions", id+".json"), file)
	f.alive[pid] = true
	if transcript {
		f.writeFile(filepath.Join(f.app.ClaudeDir, "projects", "-project", id+".jsonl"), "{}\n")
	}
}

func (f *fixture) endSession(id string, pid int) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.app.ClaudeDir, "sessions", id+".json")); err != nil {
		f.t.Fatal(err)
	}
	delete(f.alive, pid)
}

func (f *fixture) snapshotAt(seconds float64) *Boot {
	f.t.Helper()
	f.now = at(seconds)
	boot, _, err := f.app.record()
	if err != nil {
		f.t.Fatal(err)
	}
	return boot
}

func (f *fixture) reboot() {
	f.boot = BootInfo{ID: "boot-b", Time: at(9000)}
	f.now = at(9100)
}

// shutDownWith records sessions running until shutdown, then reboots.
func (f *fixture) shutDownWith(ids ...string) {
	f.t.Helper()
	for index, id := range ids {
		f.addSession(id, 100+index, 2000+float64(index))
	}
	f.snapshotAt(3000)
	for index, id := range ids {
		f.endSession(id, 100+index)
	}
	f.snapshotAt(3060)
	f.reboot()
}

func (f *fixture) run(args ...string) int {
	f.stdout.Reset()
	f.stderr.Reset()
	return f.app.Main(args)
}

func (f *fixture) writeJSON(path string, value any) {
	f.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	f.writeFile(path, string(data))
}

func (f *fixture) writeFile(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) shell(id string) []string {
	return []string{"/bin/zsh", "-l", "-i", "-c", "claude --resume " + id + "; exec /bin/zsh -l"}
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected %q in:\n%s", needle, haystack)
	}
}

func assertNotContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("did not expect %q in:\n%s", needle, haystack)
	}
}

func assertCode(t *testing.T, got, want int, f *fixture) {
	t.Helper()
	if got != want {
		t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", got, want, f.stdout.String(), f.stderr.String())
	}
}
