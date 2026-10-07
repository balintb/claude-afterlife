package main

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestRecordsOnlyLiveSessionsFromThisBoot(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.addSession(id2, 102, 2001)
	delete(f.alive, 102)
	f.addSession(id3, 103, 500)
	f.writeFile(filepath.Join(f.app.ClaudeDir, "sessions", "invalid.json"), `{"pid": 104, "sessionId": "not a valid id", "cwd": "/tmp"}`)
	f.writeFile(filepath.Join(f.app.ClaudeDir, "sessions", "broken.json"), "{not json")

	boot := f.snapshotAt(3000)

	if got := slices.Sorted(maps.Keys(boot.Sessions)); !reflect.DeepEqual(got, []string{id1}) {
		t.Fatalf("sessions = %v, want only %s", got, id1)
	}
	if !reflect.DeepEqual(boot.Running, []string{id1}) {
		t.Fatalf("running = %v", boot.Running)
	}
}

func TestKeepsFirstSeenAndMovesLastSeen(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.snapshotAt(3000)

	session := f.snapshotAt(3060).Sessions[id1]

	if !session.FirstSeen.Equal(at(3000)) || !session.LastSeen.Equal(at(3060)) {
		t.Fatalf("first seen %v, last seen %v", session.FirstSeen, session.LastSeen)
	}
}

func TestEndedSessionsStayInTheBootRecord(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.snapshotAt(3000)
	f.endSession(id1, 101)

	boot := f.snapshotAt(3060)

	if !boot.Sessions[id1].LastSeen.Equal(at(3000)) {
		t.Fatalf("last seen %v, want the earlier snapshot", boot.Sessions[id1].LastSeen)
	}
	if len(boot.Running) != 0 {
		t.Fatalf("running = %v, want none", boot.Running)
	}
}

func TestSnapshotsArePrivateAndSurviveAReload(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	written := f.snapshotAt(3000)

	path := f.app.bootPath("boot-a")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", info.Mode().Perm())
	}
	dir, _ := os.Stat(filepath.Dir(path))
	if dir.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v, want 0700", dir.Mode().Perm())
	}
	loaded, ok := loadBoot(path)
	if !ok || !loaded.Sessions[id1].StartedAt.Equal(written.Sessions[id1].StartedAt) {
		t.Fatalf("reloaded boot does not match what was written: %+v", loaded)
	}
}

func TestPrunesOldBoots(t *testing.T) {
	f := newFixture(t)
	f.app.KeepBoots = 3
	for index := range 5 {
		f.boot = BootInfo{ID: "boot-" + string(rune('0'+index)), Time: at(1000)}
		f.snapshotAt(3000 + float64(index))
		stamp := time.Unix(int64(1000+index), 0)
		if err := os.Chtimes(f.app.bootPath(f.boot.ID), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	var remaining []string
	for _, file := range f.app.bootFiles() {
		remaining = append(remaining, filepath.Base(file.path))
	}
	slices.Sort(remaining)
	if !reflect.DeepEqual(remaining, []string{"boot-2.json", "boot-3.json", "boot-4.json"}) {
		t.Fatalf("remaining boots = %v", remaining)
	}
}

func TestSnapshotTalksOnlyToATerminal(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)

	assertCode(t, f.run("snapshot"), 0, f)
	if f.stdout.Len() != 0 {
		t.Fatalf("scheduled snapshot printed %q", f.stdout.String())
	}

	f.app.StdoutTTY = true
	assertCode(t, f.run("snapshot"), 0, f)
	assertContains(t, f.stdout.String(), "1 running session(s) recorded for boot boot-a")
}
