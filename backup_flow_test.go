package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type laptop struct {
	*machine
	root    string
	remote  string
	api     string
	apiSub  string
	web     string
	notes   string
	private string
}

const (
	sessionAPI     = "aaaaaaaa-0001-4000-8000-000000000001"
	sessionAPI2    = "aaaaaaaa-0002-4000-8000-000000000002"
	sessionAPISub  = "aaaaaaaa-0003-4000-8000-000000000003"
	sessionWeb     = "aaaaaaaa-0004-4000-8000-000000000004"
	sessionNotes   = "aaaaaaaa-0005-4000-8000-000000000005"
	sessionPrivate = "aaaaaaaa-0006-4000-8000-000000000006"
)

// setupLaptop builds a machine with sessions in four routed projects (one of them a
// subfolder of a repository), one project that is never routed, Claude Code
// configuration, prompt history and a claude-afterlife snapshot.
func setupLaptop(t *testing.T, root, home string, modTime time.Time) *laptop {
	t.Helper()
	m := newMachine(t, home, "laptop-boot")
	l := &laptop{
		machine: m,
		root:    root,
		remote:  filepath.Join(root, "sessions.git"),
		api:     filepath.Join(home, "code", "api"),
		web:     filepath.Join(home, "code", "web"),
		notes:   filepath.Join(home, "notes"),
		private: filepath.Join(home, "private"),
	}
	l.apiSub = filepath.Join(l.api, "services")
	m.gitRepo(l.api, "git@github.com:me/api.git")
	m.gitRepo(l.web, "https://github.com/me/web.git")
	for _, dir := range []string{l.apiSub, l.notes, l.private} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	m.addSession(l.api, sessionAPI, 5, modTime)
	side := filepath.Join(filepath.Dir(m.sessionPath(l.api, sessionAPI)), sessionAPI, "subagents", "agent-a.jsonl")
	m.write(side, transcript(l.api, "agent-a", 0, 3, "side"), 0o600, modTime)
	m.write(filepath.Join(filepath.Dir(side), "..", "tool-results", "out.txt"), "tool output\n", 0o600, modTime)
	m.addSession(l.api, sessionAPI2, 3, modTime)
	m.addSession(l.apiSub, sessionAPISub, 2, modTime)
	m.addSession(l.web, sessionWeb, 4, modTime)
	m.write(filepath.Join(m.app.projectsRoot(), encodeProjectPath(l.web), "memory", "MEMORY.md"), "- remember the web thing\n", 0o600, modTime)
	m.addSession(l.notes, sessionNotes, 2, modTime)
	m.write(m.sessionPath(l.private, sessionPrivate), transcript(l.private, sessionPrivate, 0, 2, "PRIVATE-MARKER"), 0o600, modTime)

	claude := m.app.ClaudeDir
	m.write(filepath.Join(claude, "settings.json"), `{
  "model": "opus",
  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "bash `+home+`/.claude/hooks/stop.sh"}]}]}
}
`, 0o644, modTime)
	m.write(filepath.Join(claude, "settings.local.json"), `{"permissions":{"allow":["Bash(ls:*)"]}}`+"\n", 0o644, modTime)
	m.write(filepath.Join(claude, "CLAUDE.md"), "# Global\n\n@RTK.md\n", 0o644, modTime)
	m.write(filepath.Join(claude, "RTK.md"), "# RTK\n", 0o644, modTime)
	m.write(filepath.Join(claude, "skills", "deploy", "SKILL.md"), "---\nname: deploy\n---\n", 0o644, modTime)
	m.write(filepath.Join(claude, "skills", "deploy", "run.sh"), "#!/bin/sh\necho deploy\n", 0o755, modTime)
	m.write(filepath.Join(claude, "agents", "reviewer.md"), "reviewer\n", 0o644, modTime)
	m.write(filepath.Join(claude, "hooks", "stop.sh"), "#!/bin/sh\n", 0o755, modTime)
	m.write(filepath.Join(claude, "history.jsonl"),
		historyLine("fix the api", l.api)+historyLine("style the web", l.web)+historyLine("secret plans", l.private)+
			historyLine("no project", "")+`{"display":"half written`, 0o600, modTime)

	lastSeen := m.now.Add(-time.Hour)
	boot := Boot{Format: storeFormat, BootID: "laptop-boot", UpdatedAt: lastSeen, Running: []string{sessionAPI, sessionPrivate}, Sessions: map[string]Session{
		sessionAPI:     {ID: sessionAPI, PID: 101, Cwd: l.api, Name: "api-work", Kind: "interactive", StartedAt: lastSeen.Add(-time.Hour), FirstSeen: lastSeen.Add(-time.Hour), LastSeen: lastSeen},
		sessionWeb:     {ID: sessionWeb, PID: 102, Cwd: l.web, Name: "web-work", Kind: "interactive", StartedAt: lastSeen.Add(-time.Hour), FirstSeen: lastSeen.Add(-time.Hour), LastSeen: lastSeen},
		sessionPrivate: {ID: sessionPrivate, PID: 103, Cwd: l.private, Name: "private", Kind: "interactive", StartedAt: lastSeen.Add(-time.Hour), FirstSeen: lastSeen.Add(-time.Hour), LastSeen: lastSeen},
	}}
	if err := writePrivate(m.app.bootPath("laptop-boot"), boot); err != nil {
		t.Fatal(err)
	}
	return l
}

func historyLine(display, project string) string {
	fields := map[string]any{"display": display, "timestamp": 1}
	if project != "" {
		fields["project"] = project
	}
	line, _ := json.Marshal(fields)
	return string(line) + "\n"
}

// backUp creates the personal destination, routes ~/code and ~/notes to it, backs
// up and verifies against the remote, returning the private key file.
func (l *laptop) backUp() string {
	l.t.Helper()
	key := l.initDestination("personal", l.remote)
	l.mustRun("backup", "route", "add", "~/code/**", "personal")
	l.mustRun("backup", "route", "add", "~/notes/**", "personal")
	out := l.mustRun("backup", "run")
	assertContains(l.t, out, "Pushed to "+l.remote)
	assertContains(l.t, out, "Not backed up (no route):")
	assertContains(l.t, out, "~/private")
	assertContains(l.t, l.mustRun("backup", "verify", "-identity", key), "The local store is complete")
	assertContains(l.t, l.mustRun("backup", "verify", "-remote", "-identity", key), "Safe to wipe")
	return key
}

func (l *laptop) wipe() {
	l.t.Helper()
	for _, dir := range []string{l.app.ClaudeDir, filepath.Join(l.home, ".local")} {
		if err := os.RemoveAll(dir); err != nil {
			l.t.Fatal(err)
		}
	}
}

func modTimes(t *testing.T, root string) map[string]time.Time {
	t.Helper()
	times := map[string]time.Time{}
	for rel := range snapshot(t, root) {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		times[rel] = info.ModTime()
	}
	return times
}

func TestWipeAndRestoreToTheSamePaths(t *testing.T) {
	isolateGit(t)
	root := t.TempDir()
	home := filepath.Join(root, "Users", "me")
	l := setupLaptop(t, root, home, time.Now().Add(-48*time.Hour).Truncate(time.Second))
	key := l.backUp()
	originals := snapshot(t, l.app.ClaudeDir)
	originalTimes := modTimes(t, l.app.ClaudeDir)
	l.wipe()

	fresh := newMachine(t, home, "after-wipe")
	out := fresh.mustRun("backup", "restore", "-from", l.remote, "-identity", key, "-yes")
	assertContains(t, out, "(same path), 2 sessions")
	assertNotContains(t, out, "cleanupPeriodDays")
	restored := snapshot(t, fresh.app.ClaudeDir)

	privateFolder := encodeProjectPath(l.private)
	for rel, data := range originals {
		got, ok := restored[rel]
		switch {
		case strings.Contains(rel, privateFolder):
			if ok {
				t.Errorf("%s from the unrouted project was restored", rel)
			}
		case rel == "history.jsonl":
			want := historyLine("fix the api", l.api) + historyLine("style the web", l.web) + historyLine("no project", "")
			if string(got) != want {
				t.Errorf("history.jsonl:\n got %q\nwant %q", got, want)
			}
		case !ok:
			t.Errorf("%s was not restored", rel)
		case !bytes.Equal(got, data):
			t.Errorf("%s differs after restore:\n got %q\nwant %q", rel, got, data)
		}
	}
	for rel := range restored {
		if _, ok := originals[rel]; !ok {
			t.Errorf("restore created %s, which was not there before", rel)
		}
	}
	for _, rel := range []string{"projects/" + encodeProjectPath(l.api) + "/" + sessionAPI + ".jsonl", "skills/deploy/run.sh", "CLAUDE.md"} {
		info, err := os.Stat(filepath.Join(fresh.app.ClaudeDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(originalTimes[rel]) {
			t.Errorf("%s modified time %v, want %v", rel, info.ModTime(), originalTimes[rel])
		}
	}
	if info, _ := os.Stat(filepath.Join(fresh.app.ClaudeDir, "skills", "deploy", "run.sh")); info.Mode().Perm() != 0o755 {
		t.Errorf("run.sh lost its executable bit: %v", info.Mode())
	}

	reopen := fresh.mustRun("restore", "-terminal", "print", "-boot", "previous")
	assertContains(t, reopen, "cd "+l.api+" && claude --resume "+sessionAPI)
	assertContains(t, reopen, "cd "+l.web+" && claude --resume "+sessionWeb)
	assertNotContains(t, reopen, sessionPrivate)
	assertNotContains(t, fresh.stderr.String(), sessionPrivate)
}

func TestWipeAndRestoreToANewLayout(t *testing.T) {
	isolateGit(t)
	root := t.TempDir()
	oldHome := filepath.Join(root, "Users", "old")
	newHome := filepath.Join(root, "Users", "new")
	old := time.Now().Add(-90 * 24 * time.Hour).Truncate(time.Second)
	l := setupLaptop(t, root, oldHome, old)
	key := l.backUp()
	originals := snapshot(t, l.app.ClaudeDir)
	if err := os.RemoveAll(oldHome); err != nil {
		t.Fatal(err)
	}

	fresh := newMachine(t, newHome, "new-laptop")
	newAPI := filepath.Join(newHome, "src", "api")
	newWeb := filepath.Join(newHome, "work", "web")
	fresh.gitRepo(newAPI, "https://github.com/me/api")
	fresh.gitRepo(newWeb, "git@github.com:me/web.git")
	for _, dir := range []string{filepath.Join(newAPI, "services"), filepath.Join(newHome, "notes")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := fresh.mustRun("backup", "restore", "-from", l.remote, "-identity", key, "-search", newHome, "-yes")
	assertContains(t, out, "~/src/api  (same git remote), 2 sessions")
	assertContains(t, out, "~/src/api/services  (same git remote), 1 sessions")
	assertContains(t, out, "~/work/web  (same git remote), 1 sessions")
	assertContains(t, out, "~/notes  (home folder moved), 1 sessions")
	assertContains(t, out, "cleanupPeriodDays will be set to 3650")

	moves := map[string]string{l.api: newAPI, l.apiSub: filepath.Join(newAPI, "services"), l.web: newWeb, l.notes: filepath.Join(newHome, "notes")}
	for oldProject, newProject := range moves {
		oldFolder, newFolder := encodeProjectPath(oldProject), encodeProjectPath(newProject)
		for rel, data := range originals {
			inProject, ok := strings.CutPrefix(rel, "projects/"+oldFolder+"/")
			if !ok {
				continue
			}
			got, err := os.ReadFile(filepath.Join(fresh.app.projectsRoot(), newFolder, inProject))
			if err != nil {
				t.Errorf("%s not restored to %s: %v", rel, newFolder, err)
				continue
			}
			if filepath.Ext(rel) != ".jsonl" {
				if !bytes.Equal(got, data) {
					t.Errorf("%s changed", rel)
				}
				continue
			}
			assertOnlyCwdMoved(t, rel, data, got, oldProject, newProject)
		}
	}
	for rel := range snapshot(t, fresh.app.ClaudeDir) {
		if strings.Contains(rel, encodeProjectPath(l.private)) || strings.Contains(rel, encodeProjectPath(oldHome)) {
			t.Errorf("unexpected restored file %s", rel)
		}
	}

	settings, _ := os.ReadFile(filepath.Join(fresh.app.ClaudeDir, "settings.json"))
	assertContains(t, string(settings), "bash "+newHome+"/.claude/hooks/stop.sh")
	assertContains(t, string(settings), `"cleanupPeriodDays": 3650`)
	history, _ := os.ReadFile(filepath.Join(fresh.app.ClaudeDir, "history.jsonl"))
	assertContains(t, string(history), `"project":"`+newAPI+`"`)
	assertContains(t, string(history), `"project":"`+newWeb+`"`)
	assertNotContains(t, string(history), "secret plans")

	info, _ := os.Stat(fresh.sessionPath(newAPI, sessionAPI))
	if !info.ModTime().Equal(old) {
		t.Errorf("restored transcript is dated %v, want %v", info.ModTime(), old)
	}
	reopen := fresh.mustRun("restore", "-terminal", "print", "-boot", "previous")
	assertContains(t, reopen, "cd "+newAPI+" && claude --resume "+sessionAPI)
	assertContains(t, reopen, "cd "+newWeb+" && claude --resume "+sessionWeb)
}

func assertOnlyCwdMoved(t *testing.T, rel string, before, after []byte, oldProject, newProject string) {
	t.Helper()
	beforeLines, afterLines := completeLines(before), completeLines(after)
	if len(beforeLines) != len(afterLines) {
		t.Fatalf("%s has %d lines, want %d", rel, len(afterLines), len(beforeLines))
	}
	for i := range beforeLines {
		var b, a map[string]any
		if err := json.Unmarshal(beforeLines[i], &b); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(afterLines[i], &a); err != nil {
			t.Fatalf("%s line %d is no longer valid JSON: %v", rel, i, err)
		}
		if b["cwd"] == oldProject && a["cwd"] != newProject {
			t.Errorf("%s line %d: cwd %v, want %s", rel, i, a["cwd"], newProject)
		}
		delete(a, "cwd")
		delete(b, "cwd")
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s line %d changed beyond its cwd", rel, i)
		}
	}
}
