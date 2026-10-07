package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCurrentBootOnThisMachine(t *testing.T) {
	info, err := currentBoot()
	if err != nil {
		t.Fatal(err)
	}
	if info.ID == "" || info.Time.IsZero() || !info.Time.Before(time.Now()) {
		t.Fatalf("unexpected boot info %+v", info)
	}
	again, _ := currentBoot()
	if again.ID != info.ID {
		t.Fatalf("boot id changed between calls: %q then %q", info.ID, again.ID)
	}
}

func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("this process is not alive")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if pidAlive(cmd.Process.Pid) {
		t.Error("a finished process is alive")
	}
	if pidAlive(0) || pidAlive(-1) {
		t.Error("non-positive pids must never count as alive")
	}
}

func TestRunCommand(t *testing.T) {
	output, err := runCommand([]string{"sh", "-c", "echo ' 7 '"})
	if err != nil || output != "7" {
		t.Fatalf("output %q, err %v", output, err)
	}
	_, err = runCommand([]string{"sh", "-c", "echo boom >&2; exit 3"})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom", err)
	}
}

func TestVersionCommand(t *testing.T) {
	f := newFixture(t)

	assertCode(t, f.run("--version"), 0, f)

	if !strings.HasPrefix(f.stdout.String(), "claude-afterlife ") {
		t.Fatalf("version output %q", f.stdout.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	f := newFixture(t)

	assertCode(t, f.run("frobnicate"), 2, f)
	assertContains(t, f.stderr.String(), `unknown command "frobnicate"`)
}

func TestIsTerminalRejectsFilesAndPipes(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	for _, file := range []*os.File{devNull, reader} {
		if isTerminal(file) {
			t.Errorf("%s counts as a terminal", file.Name())
		}
	}
}
