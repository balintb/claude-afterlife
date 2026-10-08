package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func freshMachineWithoutConsent(t *testing.T) (*machine, string) {
	t.Helper()
	isolateGit(t)
	root := t.TempDir()
	m := newMachine(t, filepath.Join(root, "home"), "boot")
	delete(m.env, "CLAUDE_AFTERLIFE_ACCEPT_ALPHA")
	remote := filepath.Join(root, "remote.git")
	if _, err := runGit(root, "init", "-q", "--bare", "remote.git"); err != nil {
		t.Fatal(err)
	}
	return m, remote
}

func TestInitAsksToAcceptAlphaFirst(t *testing.T) {
	m, remote := freshMachineWithoutConsent(t)

	if code := m.run("backup", "init", "personal", remote, "-confirm-key-saved"); code != 1 {
		t.Fatal("init went ahead without accepting the alpha")
	}
	assertContains(t, m.stdout.String(), "claude-afterlife backup is ALPHA")
	assertContains(t, m.stdout.String(), "at your own risk")
	assertContains(t, m.stderr.String(), "pass -accept-alpha")
	assertNotContains(t, m.stdout.String(), "AGE-SECRET-KEY")
	if _, err := os.Stat(m.app.backupDir()); err == nil {
		t.Fatal("init created files before the alpha was accepted")
	}

	for _, answer := range []string{"\n", "n\n", "maybe\n"} {
		m.withInput(true, answer)
		if code := m.run("backup", "init", "personal", remote); code != 1 {
			t.Fatalf("answer %q was taken as yes", answer)
		}
		assertContains(t, m.stdout.String(), "Stopped. Nothing was changed.")
	}

	m.withInput(true, "yes\nsaved\n")
	out := m.mustRun("backup", "init", "personal", remote)
	assertContains(t, out, "Continue at your own risk? [y/N]")
	assertContains(t, out, "AGE-SECRET-KEY")
	config, _ := m.app.loadBackupConfig()
	if config.AlphaAccepted.IsZero() {
		t.Fatal("the acceptance was not recorded")
	}

	m.withInput(false, "")
	assertNotContains(t, m.mustRun("backup", "route", "add", "~/code/**", "personal"), "ALPHA")
	assertNotContains(t, m.mustRun("backup", "run"), "ALPHA")
	if code := m.run("backup", "init", "work", remote+"2", "-confirm-key-saved"); code != 0 {
		t.Fatalf("a second destination asked again: %s", m.stderr.String())
	}
	assertNotContains(t, m.stdout.String(), "ALPHA")
}

func TestAcceptAlphaFlagForScripts(t *testing.T) {
	m, remote := freshMachineWithoutConsent(t)
	out := m.mustRun("backup", "init", "personal", remote, "-confirm-key-saved", "-accept-alpha")
	assertContains(t, out, "claude-afterlife backup is ALPHA")
	assertContains(t, out, "AGE-SECRET-KEY")
}

func TestRestoreOnANewMachineAsksToAcceptAlpha(t *testing.T) {
	l, key := simpleLaptop(t)
	l.wipe()
	fresh := newMachine(t, l.home, "after")
	delete(fresh.env, "CLAUDE_AFTERLIFE_ACCEPT_ALPHA")

	if code := fresh.run("backup", "restore", "-from", l.remote, "-identity", key, "-yes"); code != 1 {
		t.Fatal("restore went ahead without accepting the alpha")
	}
	assertContains(t, fresh.stderr.String(), "pass -accept-alpha")
	if _, err := os.Stat(fresh.app.ClaudeDir); err == nil {
		t.Fatal("restore wrote files before the alpha was accepted")
	}

	fresh.withInput(true, "y\n")
	fresh.mustRun("backup", "restore", "-from", l.remote, "-identity", key, "-yes")
	if _, err := os.Stat(fresh.sessionPath(l.api, sessionAPI)); err != nil {
		t.Fatal("restore did not run after the alpha was accepted")
	}
	fresh.withInput(false, "")
	assertNotContains(t, fresh.mustRun("backup", "restore", "-from", l.remote, "-identity", key, "-yes"), "ALPHA")
}

func TestResumeOnlyAsksWhenItNeedsTheBackup(t *testing.T) {
	l, key := simpleLaptop(t)
	l.wipe()
	fresh := newMachine(t, l.home, "after")
	delete(fresh.env, "CLAUDE_AFTERLIFE_ACCEPT_ALPHA")

	if code := fresh.run("resume", sessionWeb, "-from", l.remote, "-identity", key); code != 1 {
		t.Fatal("resume fetched from the backup without accepting the alpha")
	}
	assertContains(t, fresh.stdout.String(), "ALPHA")
	if len(fresh.execs) != 0 {
		t.Fatal("resume started Claude Code without accepting the alpha")
	}
	fresh.mustRun("resume", sessionWeb, "-from", l.remote, "-identity", key, "-accept-alpha")
	fresh.assertResumed(sessionWeb, l.web)

	local := localLaptop(t)
	delete(local.env, "CLAUDE_AFTERLIFE_ACCEPT_ALPHA")
	local.write(local.sessionPath(local.web, "cccccccc-0001-4000-8000-000000000001"), transcript(local.web, "c", 0, 1, ""), 0o600, time.Time{})
	assertNotContains(t, local.mustRun("resume", "cccccccc-0001"), "ALPHA")
	assertNotContains(t, local.stderr.String(), "accept-alpha")
}
