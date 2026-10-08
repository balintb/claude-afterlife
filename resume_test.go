package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func (m *machine) assertResumed(id, dir string) {
	m.t.Helper()
	want := []execCall{{dir: dir, argv: []string{"/bin/sh", "-c", "claude --resume " + id}}}
	if !reflect.DeepEqual(m.execs, want) {
		m.t.Fatalf("ran %+v, want %+v", m.execs, want)
	}
}

func localLaptop(t *testing.T) *laptop {
	t.Helper()
	isolateGit(t)
	root := t.TempDir()
	return setupLaptop(t, root, filepath.Join(root, "Users", "me"), time.Now().Add(-time.Hour).Truncate(time.Second))
}

func TestResumeASessionFromAnyFolder(t *testing.T) {
	l := localLaptop(t)
	l.mustRun("resume", "aaaaaaaa-0004")
	l.assertResumed(sessionWeb, l.web)
	assertContains(t, l.stderr.String(), "Resuming aaaaaaaa in ~/code/web")
}

func TestResumeTakesTheFullIDLikeClaude(t *testing.T) {
	for _, form := range [][]string{
		{"resume", "aaaaaaaa-0004-4000-8000-000000000004"},
		{"--resume", "aaaaaaaa-0004-4000-8000-000000000004"},
		{"-r", "aaaaaaaa-0004-4000-8000-000000000004"},
		{"resume", "AAAAAAAA-0004-4000-8000-000000000004"},
		{"--resume", "aaaaaaaa-0004"},
	} {
		l := localLaptop(t)
		l.mustRun(form...)
		l.assertResumed(sessionWeb, l.web)
	}
}

func TestResumeFindsSessionsInSubfolderProjects(t *testing.T) {
	l := localLaptop(t)
	l.mustRun("resume", sessionAPISub)
	l.assertResumed(sessionAPISub, l.apiSub)
}

func TestResumeRefusesAmbiguousIDs(t *testing.T) {
	l := localLaptop(t)
	if code := l.run("resume", "aaaaaaaa"); code != 1 {
		t.Fatal("an ambiguous prefix was accepted")
	}
	assertContains(t, l.stderr.String(), "several sessions start with that")
	assertContains(t, l.stderr.String(), sessionAPI)
	if len(l.execs) != 0 {
		t.Fatal("ran Claude Code for an ambiguous id")
	}
}

func TestResumeRefusesARunningSession(t *testing.T) {
	l := localLaptop(t)
	registry, _ := json.Marshal(claudeSessionFile{PID: 4242, SessionID: sessionWeb, Cwd: l.web, StartedAt: float64(l.now.UnixMilli())})
	l.write(filepath.Join(l.app.ClaudeDir, "sessions", "4242.json"), string(registry), 0o600, time.Time{})
	l.alive[4242] = true
	if code := l.run("resume", sessionWeb); code != 1 {
		t.Fatal("resumed a session that is already running")
	}
	assertContains(t, l.stderr.String(), "is running (process 4242")
	if len(l.execs) != 0 {
		t.Fatal("ran a second copy of a running session")
	}
}

func TestResumePrintsInsteadOfRunning(t *testing.T) {
	l := localLaptop(t)
	out := l.mustRun("resume", sessionWeb, "-print", "-claude-command", "claude --model opus")
	if out != "cd "+l.web+" && claude --model opus --resume "+sessionWeb+"\n" {
		t.Fatalf("printed %q", out)
	}
	if len(l.execs) != 0 {
		t.Fatal("-print ran Claude Code")
	}
}

func TestResumeInAnotherFolderCopiesTheSession(t *testing.T) {
	l := localLaptop(t)
	elsewhere := filepath.Join(l.home, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	l.mustRun("resume", sessionAPI, "-in", elsewhere)
	l.assertResumed(sessionAPI, elsewhere)
	copied, err := os.ReadFile(l.sessionPath(elsewhere, sessionAPI))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(l.sessionPath(l.api, sessionAPI))
	assertOnlyCwdMoved(t, "copied transcript", original, copied, l.api, elsewhere)
	side := filepath.Join(l.app.projectsRoot(), encodeProjectPath(elsewhere), sessionAPI, "subagents", "agent-a.jsonl")
	if _, err := os.Stat(side); err != nil {
		t.Fatal("the subagent transcript was not copied along")
	}
}

func TestResumeExplainsMissingSessionsAndFolders(t *testing.T) {
	l := localLaptop(t)
	if code := l.run("resume", "deadbeef"); code != 1 {
		t.Fatal("resumed a session that does not exist")
	}
	assertContains(t, l.stderr.String(), "no session deadbeef on this machine; if it is in a backup, pass -from <git-remote>")
	if err := os.RemoveAll(l.web); err != nil {
		t.Fatal(err)
	}
	if code := l.run("resume", sessionWeb); code != 1 {
		t.Fatal("resumed into a folder that no longer exists")
	}
	assertContains(t, l.stderr.String(), "pass -in <folder>")
	if code := l.run("resume", "xy"); code != 2 && code != 1 {
		t.Fatal("a two-character id was accepted")
	}
}

func TestResumeBringsBackASessionClaudeCodeDeleted(t *testing.T) {
	l, key := simpleLaptop(t)
	path := l.sessionPath(l.web, sessionWeb)
	original, _ := os.ReadFile(path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if code := l.run("resume", sessionWeb); code != 1 {
		t.Fatal("restored from the backup without the private key")
	}
	assertContains(t, l.stderr.String(), "-identity")

	l.execs = nil
	l.mustRun("resume", sessionWeb, "-identity", key)
	l.assertResumed(sessionWeb, l.web)
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("the session was not restored from the backup")
	}
	if string(restored) != string(original) {
		t.Fatal("the restored session differs from the original")
	}
	if info, _ := os.Stat(path); time.Since(info.ModTime()) > time.Hour {
		t.Error("the restored session keeps an old date, so Claude Code's cleanup could delete it straight away")
	}
	assertContains(t, l.stderr.String(), "Restored session aaaaaaaa from the backup")
}

func TestResumeFromABackupOnANewMachine(t *testing.T) {
	isolateGit(t)
	root := t.TempDir()
	oldHome := filepath.Join(root, "Users", "old")
	l := setupLaptop(t, root, oldHome, time.Now().Add(-time.Hour).Truncate(time.Second))
	key := l.backUp()
	original, _ := os.ReadFile(l.sessionPath(l.api, sessionAPI))
	if err := os.RemoveAll(oldHome); err != nil {
		t.Fatal(err)
	}

	newHome := filepath.Join(root, "Users", "new")
	fresh := newMachine(t, newHome, "new-laptop")
	newAPI := filepath.Join(newHome, "src", "api")
	fresh.gitRepo(newAPI, "https://github.com/me/api")
	fresh.mustRun("resume", "aaaaaaaa-0001", "-from", l.remote, "-identity", key, "-search", newHome)
	fresh.assertResumed(sessionAPI, newAPI)
	restored, err := os.ReadFile(fresh.sessionPath(newAPI, sessionAPI))
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyCwdMoved(t, "resumed transcript", original, restored, l.api, newAPI)
	side := filepath.Join(fresh.app.projectsRoot(), encodeProjectPath(newAPI), sessionAPI, "subagents", "agent-a.jsonl")
	if _, err := os.Stat(side); err != nil {
		t.Fatal("the subagent transcript was not restored with the session")
	}
	if _, err := os.Stat(fresh.sessionPath(newAPI, sessionAPI2)); err == nil {
		t.Fatal("resume restored other sessions too")
	}

	fresh.execs = nil
	if code := fresh.run("resume", sessionWeb, "-from", l.remote, "-identity", key, "-search", newHome); code != 1 {
		t.Fatal("resumed without knowing where the project lives")
	}
	assertContains(t, fresh.stderr.String(), "pass -in <folder>")
	chosen := filepath.Join(newHome, "web")
	if err := os.MkdirAll(chosen, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh.mustRun("resume", sessionWeb, "-from", l.remote, "-identity", key, "-in", chosen)
	fresh.assertResumed(sessionWeb, chosen)

	other, _, _ := newBackupKey()
	wrongKey := filepath.Join(root, "wrong.key")
	fresh.write(wrongKey, other, 0o600, time.Time{})
	if code := fresh.run("resume", sessionNotes, "-from", l.remote, "-identity", wrongKey, "-in", chosen); code != 1 {
		t.Fatal("resumed with the wrong key")
	}
	assertContains(t, fresh.stderr.String(), "cannot open the backup")
}
