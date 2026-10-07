package main

import (
	"testing"
)

func TestAutoReopensSessionsLostWhenTheTerminalQuit(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.addSession(id2, 102, 2001)
	f.snapshotAt(3000)
	f.endSession(id1, 101)
	f.endSession(id2, 102)
	f.snapshotAt(3060)
	f.now = at(5000)

	assertCode(t, f.run("restore", "-terminal", "print"), 0, f)

	want := f.typed(id1) + "\n" + f.typed(id2) + "\n"
	if f.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", f.stdout.String(), want)
	}
	assertContains(t, f.stderr.String(), "Ended during this boot")
}

func TestAutoOffersOnlyTheLatestLoss(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.addSession(id2, 102, 2001)
	f.snapshotAt(3000)
	f.endSession(id1, 101)
	f.snapshotAt(6600)
	f.endSession(id2, 102)
	f.snapshotAt(6660)

	f.run("restore", "-terminal", "print")

	if want := f.typed(id2) + "\n"; f.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", f.stdout.String(), want)
	}
}

func TestAutoOffersTheRebootAndLaterLossesTogether(t *testing.T) {
	f := newFixture(t)
	f.shutDownWith(id1)
	f.addSession(id2, 201, 9050)
	f.snapshotAt(9200)
	f.endSession(id2, 201)

	f.run("restore", "-terminal", "print")

	if want := f.typed(id1) + "\n" + f.typed(id2) + "\n"; f.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", f.stdout.String(), want)
	}
	assertContains(t, f.stderr.String(), "Running before the reboot")
	assertContains(t, f.stderr.String(), "Ended during this boot")
}

func TestAutoForgetsTheRebootOnceRestored(t *testing.T) {
	f := newFixture(t)
	f.paths["osascript"] = "/fake/bin/osascript"
	f.shutDownWith(id1)

	assertCode(t, f.run("restore", "-terminal", "ghostty", "-yes"), 0, f)
	if len(f.calls) != 1 {
		t.Fatalf("calls = %q", f.calls)
	}

	f.now = at(9500)
	assertCode(t, f.run("restore", "-terminal", "print"), 0, f)
	assertContains(t, f.stderr.String(), "Nothing to reopen.")
	assertNotContains(t, f.stderr.String(), "Running before the reboot")

	assertCode(t, f.run("restore", "-boot", "previous", "-terminal", "print"), 0, f)
	assertContains(t, f.stdout.String(), id1)
}

func TestRestoredSessionsCanBeRestoredAgainAfterTheTerminalQuits(t *testing.T) {
	f := newFixture(t)
	f.paths["osascript"] = "/fake/bin/osascript"
	f.shutDownWith(id1)
	assertCode(t, f.run("restore", "-terminal", "ghostty", "-yes"), 0, f)

	f.addSession(id1, 301, 9150)
	f.snapshotAt(9200)
	f.endSession(id1, 301)
	f.snapshotAt(9260)
	f.now = at(9500)

	assertCode(t, f.run("restore", "-terminal", "print"), 0, f)

	if want := f.typed(id1) + "\n"; f.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", f.stdout.String(), want)
	}
	assertContains(t, f.stderr.String(), "Ended during this boot")
	assertNotContains(t, f.stderr.String(), "Running before the reboot")
}

func TestDeclinedRestoreKeepsTheRebootOnOffer(t *testing.T) {
	f := newFixture(t)
	f.paths["osascript"] = "/fake/bin/osascript"
	f.shutDownWith(id1)
	f.app.StdinTTY = true

	assertCode(t, f.run("restore", "-terminal", "ghostty"), 1, f)
	f.run("restore", "-terminal", "print")

	assertContains(t, f.stdout.String(), id1)
}

func TestClearedConversationInARunningProcessIsNotOffered(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.snapshotAt(3000)
	f.endSession(id1, 101)
	f.addSession(id2, 101, 2000)
	f.snapshotAt(3060)

	f.run("restore", "-terminal", "print")

	if f.stdout.Len() != 0 {
		t.Fatalf("offered %q from a process that is still running", f.stdout.String())
	}
}

func TestRestoreWithoutAnySnapshot(t *testing.T) {
	f := newFixture(t)

	assertCode(t, f.run("restore", "-terminal", "print"), 1, f)
	assertContains(t, f.stderr.String(), "no snapshots in")
}
