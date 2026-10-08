package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (m *machine) useConfig(content string) error {
	m.t.Helper()
	path := filepath.Join(m.home, ".config", "claude-afterlife", "config.toml")
	m.write(path, content, 0o644, time.Time{})
	return m.app.useConfigFile()
}

func (m *machine) mustUseConfig(content string) {
	m.t.Helper()
	if err := m.useConfig(content); err != nil {
		m.t.Fatal(err)
	}
}

func TestConfigFileSetsRestoreDefaults(t *testing.T) {
	l := localLaptop(t)
	l.mustUseConfig(`
[restore]
terminal = "tmux"
claude_command = "claude --model opus"
recent = "2h"
include_background = true
`)
	assertContains(t, l.mustRun("restore", "-dry-run"), "would reopen 3 session(s) in tmux")
	out := l.mustRun("restore", "-terminal", "print")
	assertContains(t, out, "claude --model opus --resume "+sessionAPI)
	if l.app.defaultRecent() != 2*time.Hour {
		t.Errorf("recent = %v", l.app.defaultRecent())
	}
}

func TestFlagsAndEnvironmentBeatTheConfigFile(t *testing.T) {
	l := localLaptop(t)
	l.mustUseConfig("[restore]\nterminal = \"tmux\"\nclaude_command = \"claude --from-file\"\n")
	l.env["CLAUDE_AFTERLIFE_TERMINAL"] = "wezterm"
	l.env["CLAUDE_AFTERLIFE_CLAUDE"] = "claude --from-env"
	assertContains(t, l.mustRun("restore", "-dry-run"), "in wezterm")
	out := l.mustRun("restore", "-terminal", "print", "-claude-command", "claude --from-flag")
	assertContains(t, out, "claude --from-flag --resume")
	assertContains(t, l.mustRun("resume", sessionWeb, "-print"), "claude --from-env --resume")
}

func TestConfigFileSetsFoldersUnlessTheEnvironmentDoes(t *testing.T) {
	isolateGit(t)
	m := newMachine(t, t.TempDir(), "boot")
	m.mustUseConfig("claude_dir = \"~/elsewhere/claude\"\nstate_dir = \"~/elsewhere/state\"\n")
	if m.app.ClaudeDir != filepath.Join(m.home, "elsewhere", "claude") || m.app.StateDir != filepath.Join(m.home, "elsewhere", "state") {
		t.Fatalf("folders not taken from the file: %s, %s", m.app.ClaudeDir, m.app.StateDir)
	}
	m.env["CLAUDE_CONFIG_DIR"] = "/from/env"
	m.app.ClaudeDir = "/from/env"
	m.mustUseConfig("claude_dir = \"~/elsewhere/claude\"\n")
	if m.app.ClaudeDir != "/from/env" {
		t.Fatalf("the config file overrode CLAUDE_CONFIG_DIR: %s", m.app.ClaudeDir)
	}
	m.env["CLAUDE_AFTERLIFE_CONFIG"] = filepath.Join(m.home, "custom.toml")
	m.write(m.env["CLAUDE_AFTERLIFE_CONFIG"], "[restore]\nterminal = \"kitty\"\n", 0o644, time.Time{})
	if err := m.app.useConfigFile(); err != nil || m.app.defaultTerminal() != "kitty" {
		t.Fatalf("CLAUDE_AFTERLIFE_CONFIG was not used: %v, %s", err, m.app.defaultTerminal())
	}
}

func TestConfigFileMistakesAreReported(t *testing.T) {
	cases := map[string]string{
		"[restore]\nterminl = \"tmux\"\n":                              "unknown setting restore.terminl",
		"[restore]\nrecent = \"ten minutes\"\n":                        "is not a duration",
		"[restore]\nrecent = \"-5m\"\n":                                "must be positive",
		"[restore]\nterminal = \"hyper\"\n":                            `restore.terminal "hyper" is not one of`,
		"[[backup.routes]]\npath = \"~/code/**\"\n":                    "needs a path and a destination",
		"[[backup.routes]]\npath = \"code/**\"\ndestination = \"x\"\n": "must be an absolute path",
		"[restore\n": "config.toml",
	}
	for content, want := range cases {
		m := newMachine(t, t.TempDir(), "boot")
		err := m.useConfig(content)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("config %q gave %v, want an error containing %q", content, err, want)
		}
	}
}

func TestRoutesFromTheConfigFile(t *testing.T) {
	isolateGit(t)
	root := t.TempDir()
	l := setupLaptop(t, root, filepath.Join(root, "Users", "me"), time.Now())
	l.mustUseConfig("[[backup.routes]]\npath = \"~/code/web\"\ndestination = \"personal\"\n")
	assertContains(t, l.mustRun("backup", "status"), `(no destination "personal" yet`)
	key := l.initDestination("personal", l.remote)
	l.mustRun("backup", "route", "add", "~/notes", "personal")
	status := l.mustRun("backup", "status")
	assertContains(t, status, "~/code/web  ->  personal  (config file)")
	assertContains(t, status, "~/notes  ->  personal\n")
	assertContains(t, l.mustRun("backup", "run"), "personal: 2 sessions in 2 projects")
	_, _, index := l.openStore("personal", key)
	entryFor(t, index, sessionWeb+".jsonl")
	entryFor(t, index, sessionNotes+".jsonl")

	if code := l.run("backup", "route", "remove", "~/code/web"); code != 1 {
		t.Fatal("a route from the config file was removed from the command line")
	}
	assertContains(t, l.stderr.String(), "is set in")
}

func TestSettingsDestinationFromTheConfigFile(t *testing.T) {
	isolateGit(t)
	root := t.TempDir()
	l := setupLaptop(t, root, filepath.Join(root, "Users", "me"), time.Now())
	l.mustUseConfig("[backup]\nsettings = \"none\"\n\n[[backup.routes]]\npath = \"~/code/web\"\ndestination = \"personal\"\n")
	key := l.initDestination("personal", l.remote)
	assertContains(t, l.mustRun("backup", "status"), "Claude Code settings and skills  ->  not backed up")
	if code := l.run("backup", "route", "config", "personal"); code != 1 {
		t.Fatal("the settings destination was changed although the config file sets it")
	}
	l.mustRun("backup", "run")
	_, _, index := l.openStore("personal", key)
	for _, entry := range index.Files {
		if strings.HasPrefix(entry.Path, "claude/settings") || strings.HasPrefix(entry.Path, "claude/skills") {
			t.Errorf("%s was backed up although backup.settings is none", entry.Path)
		}
	}
}

func TestConfigInitWritesATemplateThatChangesNothing(t *testing.T) {
	isolateGit(t)
	m := newMachine(t, t.TempDir(), "boot")
	assertContains(t, m.mustRun("config"), "not found, using the defaults")
	assertContains(t, m.mustRun("config", "init"), "Wrote ~/.config/claude-afterlife/config.toml")
	if err := m.app.useConfigFile(); err != nil {
		t.Fatalf("the template does not parse: %v", err)
	}
	if m.app.defaultTerminal() != "auto" || m.app.defaultClaudeCommand() != "claude" || m.app.defaultRecent() != defaultRecent || m.app.defaultInterval() != time.Minute {
		t.Fatal("the commented template changed a default")
	}
	if code := m.run("config", "init"); code != 1 {
		t.Fatal("config init overwrote an existing file")
	}
	out := m.mustRun("config")
	assertContains(t, out, "(in use)")
	assertContains(t, out, "restore.terminal")
	data, _ := os.ReadFile(filepath.Join(m.home, ".config", "claude-afterlife", "config.toml"))
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") && !strings.HasPrefix(line, "[") {
			t.Errorf("the template sets %q instead of leaving it commented", line)
		}
	}
}

func TestBackupSearchFromTheConfigFile(t *testing.T) {
	m := newMachine(t, t.TempDir(), "boot")
	m.mustUseConfig("[backup]\nsearch = [\"~/src\", \"/srv/repos\"]\n")
	if got := m.app.defaultSearch(); len(got) != 2 || got[0] != "~/src" {
		t.Fatalf("search = %v", got)
	}
}
