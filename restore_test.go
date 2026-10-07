package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReopensSessionsRunningAtShutdown(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.addSession(id2, 102, 2001)
	f.snapshotAt(3000)
	f.endSession(id2, 102)
	f.snapshotAt(6600)
	f.endSession(id1, 101)
	f.snapshotAt(6660)
	f.reboot()

	assertCode(t, f.run("restore", "-terminal", "print"), 0, f)

	if want := "cd " + f.work + " && claude --resume " + id1 + "\n"; f.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", f.stdout.String(), want)
	}
}

func TestAllIncludesSessionsThatEndedEarlier(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.addSession(id2, 102, 2001)
	f.snapshotAt(3000)
	f.endSession(id2, 102)
	f.snapshotAt(6600)
	f.reboot()

	f.run("restore", "-terminal", "print", "-all")

	assertContains(t, f.stdout.String(), id1)
	assertContains(t, f.stdout.String(), id2)
}

func TestSkipsSessionsAlreadyRunning(t *testing.T) {
	f := newFixture(t)
	f.shutDownWith(id1, id2)
	f.addSession(id1, 201, 9050)

	f.run("restore", "-terminal", "print")

	assertNotContains(t, f.stdout.String(), id1)
	assertContains(t, f.stdout.String(), id2)
	assertContains(t, f.stderr.String(), "already running")
}

func TestSkipsMissingDirectoryAndTranscript(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000, withCwd(filepath.Join(f.root, "gone")))
	f.addSession(id2, 102, 2001, withoutTranscript())
	f.snapshotAt(3000)
	f.reboot()

	assertCode(t, f.run("restore", "-terminal", "print"), 0, f)

	if f.stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", f.stdout.String())
	}
	for _, want := range []string{"missing directory", "missing transcript", "Nothing to reopen."} {
		assertContains(t, f.stderr.String(), want)
	}
}

func TestBackgroundSessionsNeedAFlag(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000, withKind("bg"))
	f.snapshotAt(3000)
	f.reboot()

	f.run("restore", "-terminal", "print")
	assertNotContains(t, f.stdout.String(), id1)

	f.run("restore", "-terminal", "print", "-include-background")
	assertContains(t, f.stdout.String(), id1)
}

func TestClearedConversationReopensOnlyTheNewestID(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.snapshotAt(3000)
	f.endSession(id1, 101)
	f.addSession(id2, 101, 2000)
	f.snapshotAt(3060)
	f.reboot()

	f.run("restore", "-terminal", "print")

	if want := "cd " + f.work + " && claude --resume " + id2 + "\n"; f.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", f.stdout.String(), want)
	}
}

func TestQuotesDirectoriesAndUsesACustomCommand(t *testing.T) {
	f := newFixture(t)
	spaced := filepath.Join(f.root, "my work")
	f.writeFile(filepath.Join(spaced, ".keep"), "")
	f.addSession(id1, 101, 2000, withCwd(spaced))
	f.snapshotAt(3000)
	f.reboot()

	f.run("restore", "-terminal", "print", "-claude-command", "claude --model opus")

	if want := "cd '" + spaced + "' && claude --model opus --resume " + id1 + "\n"; f.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", f.stdout.String(), want)
	}
}

func TestNeedsASnapshotFromAnEarlierBoot(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.snapshotAt(3000)

	assertCode(t, f.run("restore", "-boot", "previous", "-terminal", "print"), 1, f)
	assertContains(t, f.stderr.String(), "no snapshot from an earlier boot")
}

func TestCurrentBootRestoresSessionsClosedThisBoot(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.addSession(id2, 102, 2001)
	f.snapshotAt(3000)
	f.endSession(id2, 102)
	f.snapshotAt(3060)

	f.run("restore", "-boot", "current", "-terminal", "print")

	assertContains(t, f.stdout.String(), id2)
	assertNotContains(t, f.stdout.String(), id1)
}

func TestRejectsUnknownTerminal(t *testing.T) {
	f := newFixture(t)

	assertCode(t, f.run("restore", "-terminal", "hyper"), 2, f)
	assertContains(t, f.stderr.String(), `Unknown terminal "hyper"`)
}

func TestListMarksRestoreCandidates(t *testing.T) {
	f := newFixture(t)
	f.shutDownWith(id1)

	assertCode(t, f.run("list"), 0, f)

	assertContains(t, f.stdout.String(), "boot-a")
	assertContains(t, f.stdout.String(), "1 restore candidate(s)")
	assertContains(t, f.stdout.String(), "+ "+id1[:8])
}

func spawnFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.paths["thinkterm"] = "/fake/bin/thinkterm"
	f.paths["tmux"] = "/fake/bin/tmux"
	f.shutDownWith(id1, id2)
	return f
}

func TestOpensOneNewWindowThenTabsInIt(t *testing.T) {
	f := spawnFixture(t)

	assertCode(t, f.run("restore", "-terminal", "thinkterm", "-yes"), 0, f)

	assertContains(t, f.stdout.String(), "Reopened 2 of 2 session(s) in thinkterm.")
	want := [][]string{
		append([]string{"/fake/bin/thinkterm", "cli", "spawn", "--cwd", f.work, "--new-window", "--"}, f.shell(id1)...),
		append([]string{"/fake/bin/thinkterm", "cli", "spawn", "--cwd", f.work, "--pane-id", "42", "--"}, f.shell(id2)...),
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls:\n%q\nwant:\n%q", f.calls, want)
	}
}

func TestUsesTheCurrentWindowInsideTheTerminal(t *testing.T) {
	f := spawnFixture(t)
	f.env["WEZTERM_PANE"] = "7"

	f.run("restore", "-terminal", "thinkterm", "-y")

	want := [][]string{
		append([]string{"/fake/bin/thinkterm", "cli", "spawn", "--cwd", f.work, "--"}, f.shell(id1)...),
		append([]string{"/fake/bin/thinkterm", "cli", "spawn", "--cwd", f.work, "--"}, f.shell(id2)...),
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls:\n%q\nwant:\n%q", f.calls, want)
	}
}

func TestNewWindowFlagOverridesTheCurrentWindow(t *testing.T) {
	f := spawnFixture(t)
	f.env["WEZTERM_PANE"] = "7"

	f.run("restore", "-terminal", "thinkterm", "-yes", "-new-window")

	assertContains(t, strings.Join(f.calls[0], " "), "--new-window")
}

func TestTmuxOpensAWindowPerSession(t *testing.T) {
	f := spawnFixture(t)

	f.run("restore", "-terminal", "tmux", "-yes")

	want := [][]string{
		append([]string{"/fake/bin/tmux", "new-window", "-c", f.work}, f.shell(id1)...),
		append([]string{"/fake/bin/tmux", "new-window", "-c", f.work}, f.shell(id2)...),
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls:\n%q\nwant:\n%q", f.calls, want)
	}
}

func TestDryRunOpensNothing(t *testing.T) {
	f := spawnFixture(t)

	assertCode(t, f.run("restore", "-terminal", "thinkterm", "-dry-run"), 0, f)

	assertContains(t, f.stdout.String(), "would reopen 2 session(s)")
	if len(f.calls) != 0 {
		t.Fatalf("dry run ran %q", f.calls)
	}
}

func TestAsksBeforeOpening(t *testing.T) {
	f := spawnFixture(t)
	f.app.StdinTTY = true
	f.app.Stdin = strings.NewReader("n\n")

	assertCode(t, f.run("restore", "-terminal", "thinkterm"), 1, f)

	assertContains(t, f.stdout.String(), "Reopen 2 session(s) in thinkterm? [Y/n]")
	if len(f.calls) != 0 {
		t.Fatalf("declined restore ran %q", f.calls)
	}
}

func TestRefusesToOpenWithoutConfirmation(t *testing.T) {
	f := spawnFixture(t)

	assertCode(t, f.run("restore", "-terminal", "thinkterm"), 1, f)

	assertContains(t, f.stderr.String(), "pass -yes")
	if len(f.calls) != 0 {
		t.Fatalf("opened %q without confirmation", f.calls)
	}
}

func TestReportsFailedSpawns(t *testing.T) {
	f := spawnFixture(t)
	f.runErr = errors.New("boom")

	assertCode(t, f.run("restore", "-terminal", "thinkterm", "-yes"), 1, f)

	assertContains(t, f.stderr.String(), "boom")
	assertContains(t, f.stdout.String(), "Reopened 0 of 2")
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/plain/path":    "/plain/path",
		"/with space":    "'/with space'",
		"it's":           `'it'"'"'s'`,
		"$(rm -rf ~)":    "'$(rm -rf ~)'",
		"":               "''",
		"a-b_c.d@e%f+g=": "a-b_c.d@e%f+g=",
	}
	for input, want := range cases {
		if got := shellQuote(input); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", input, got, want)
		}
	}
}
