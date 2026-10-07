package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRenderPlistEscapesValues(t *testing.T) {
	plist := string(renderPlist("local.test", []string{"/Apps & Tools/claude-afterlife", "snapshot"},
		map[string]string{"CLAUDE_AFTERLIFE_DIR": "/state/<dir>"}, 90*time.Second, "/state/error.log"))

	assertContains(t, plist, "<string>/Apps &amp; Tools/claude-afterlife</string>")
	assertContains(t, plist, "<string>/state/&lt;dir&gt;</string>")
	assertContains(t, plist, "<integer>90</integer>")

	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil is only available on macOS")
	}
	path := filepath.Join(t.TempDir(), "agent.plist")
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v\n%s", err, output)
	}
}

func TestInstallWritesAndStartsTheAgent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd agents exist only on macOS")
	}
	f := newFixture(t)

	assertCode(t, f.run("install"), 0, f)

	plist, err := os.ReadFile(f.app.agentPlistPath())
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, string(plist), "<string>/bin/sh</string>")
	assertContains(t, string(plist), "<string>/usr/local/bin/claude-afterlife</string>")
	assertContains(t, string(plist), "<string>"+f.app.agentPlistPath()+"</string>")
	assertContains(t, string(plist), "<string>"+f.app.StateDir+"</string>")
	assertNotContains(t, string(plist), "CLAUDE_CONFIG_DIR")
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	want := [][]string{
		{"launchctl", "bootout", domain + "/" + agentLabel},
		{"launchctl", "bootstrap", domain, f.app.agentPlistPath()},
	}
	if fmt.Sprint(f.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
}

func TestInstallCanSkipStartingTheAgent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd agents exist only on macOS")
	}
	f := newFixture(t)
	f.env["CLAUDE_CONFIG_DIR"] = f.app.ClaudeDir

	assertCode(t, f.run("install", "-no-load"), 0, f)

	if len(f.calls) != 0 {
		t.Fatalf("-no-load ran %q", f.calls)
	}
	plist, _ := os.ReadFile(f.app.agentPlistPath())
	assertContains(t, string(plist), "<key>CLAUDE_CONFIG_DIR</key>")
}

func TestInstallPointsHomebrewCopiesAtTheOptLink(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd agents exist only on macOS")
	}
	f := newFixture(t)
	prefix := filepath.Join(f.root, "homebrew")
	cellar := filepath.Join(prefix, "Cellar", "claude-afterlife", "0.1.0", "bin", "claude-afterlife")
	opt := filepath.Join(prefix, "opt", "claude-afterlife", "bin", "claude-afterlife")
	f.writeFile(cellar, "")
	f.writeFile(opt, "")
	f.app.Executable = func() (string, error) { return cellar, nil }

	assertCode(t, f.run("install", "-no-load"), 0, f)

	plist, _ := os.ReadFile(f.app.agentPlistPath())
	assertContains(t, string(plist), "<string>"+opt+"</string>")
	assertNotContains(t, string(plist), "Cellar")
}

func TestStableExecutableKeepsOtherPaths(t *testing.T) {
	for _, path := range []string{
		"/Users/me/go/bin/claude-afterlife",
		"/opt/homebrew/Cellar/claude-afterlife/0.1.0/bin/claude-afterlife-missing-opt",
	} {
		if got := stableExecutable(path); got != path {
			t.Errorf("stableExecutable(%q) = %q", path, got)
		}
	}
}

func TestUninstallRemovesTheAgentAndCanPurge(t *testing.T) {
	f := newFixture(t)
	f.addSession(id1, 101, 2000)
	f.snapshotAt(3000)
	f.writeFile(f.app.agentPlistPath(), "<plist/>")

	assertCode(t, f.run("uninstall", "-purge"), 0, f)

	for _, path := range []string{f.app.StateDir} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s still exists", path)
		}
	}
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat(f.app.agentPlistPath()); err == nil {
			t.Error("launchd agent still exists")
		}
	}
}

func TestUninstallRefusesToPurgeHome(t *testing.T) {
	f := newFixture(t)
	f.app.StateDir = f.root

	assertCode(t, f.run("uninstall", "-purge"), 1, f)

	assertContains(t, f.stderr.String(), "refusing to delete")
	if _, err := os.Stat(f.work); err != nil {
		t.Fatal("purge deleted the home directory")
	}
}

type agentScriptRun struct {
	dir       string
	binary    string
	plist     string
	marker    string
	launchctl string
	calls     string
}

func newAgentScriptRun(t *testing.T) *agentScriptRun {
	dir := t.TempDir()
	r := &agentScriptRun{
		dir:       dir,
		binary:    filepath.Join(dir, "claude-afterlife"),
		plist:     filepath.Join(dir, "agent.plist"),
		marker:    filepath.Join(dir, "snapshots"),
		launchctl: filepath.Join(dir, "launchctl"),
		calls:     filepath.Join(dir, "launchctl-calls"),
	}
	writeExecutable(t, r.launchctl, "#!/bin/sh\necho \"$@\" >> "+shellQuote(r.calls)+"\n")
	if err := os.WriteFile(r.plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *agentScriptRun) installBinary(t *testing.T) {
	writeExecutable(t, r.binary, "#!/bin/sh\necho \"$@\" >> "+shellQuote(r.marker)+"\n")
}

func (r *agentScriptRun) command(recheckSeconds string) *exec.Cmd {
	program := agentProgram(r.binary, r.plist, "local.test", 0)
	program[len(program)-1] = recheckSeconds
	return exec.Command(program[0], append(program[1:], r.launchctl)...)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestAgentScriptSnapshotsWhileTheBinaryExists(t *testing.T) {
	r := newAgentScriptRun(t)
	r.installBinary(t)

	if output, err := r.command("0").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}

	snapshots, _ := os.ReadFile(r.marker)
	if string(snapshots) != "snapshot\n" {
		t.Fatalf("binary ran with %q, want snapshot", snapshots)
	}
	if _, err := os.Stat(r.plist); err != nil {
		t.Fatal("the plist was removed while the binary exists")
	}
}

func TestAgentScriptRemovesItselfOnceTheBinaryIsGone(t *testing.T) {
	r := newAgentScriptRun(t)

	if output, err := r.command("0").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}

	if _, err := os.Stat(r.plist); err == nil {
		t.Fatal("the plist is still there")
	}
	calls, _ := os.ReadFile(r.calls)
	if want := fmt.Sprintf("bootout gui/%d/local.test\n", os.Getuid()); string(calls) != want {
		t.Fatalf("launchctl called with %q, want %q", calls, want)
	}
}

func TestAgentScriptRidesOutABriefGap(t *testing.T) {
	r := newAgentScriptRun(t)
	cmd := r.command("2")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	r.installBinary(t)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(r.plist); err != nil {
		t.Fatal("the plist was removed although the binary came back")
	}
	if _, err := os.Stat(r.calls); err == nil {
		t.Fatal("launchctl was called although the binary came back")
	}
	snapshots, _ := os.ReadFile(r.marker)
	if string(snapshots) != "snapshot\n" {
		t.Fatalf("binary ran with %q, want snapshot", snapshots)
	}
}

// TestLaunchdAgentRemovesItself loads a throwaway launchd agent with its own label,
// so it never touches a real installation. It only runs on request.
func TestLaunchdAgentRemovesItself(t *testing.T) {
	if os.Getenv("CLAUDE_AFTERLIFE_LAUNCHD_TEST") == "" {
		t.Skip("set CLAUDE_AFTERLIFE_LAUNCHD_TEST=1 to load a throwaway launchd agent")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("launchd agents exist only on macOS")
	}
	dir := t.TempDir()
	label := fmt.Sprintf("local.claude-afterlife-selftest-%d", os.Getpid())
	binary := filepath.Join(dir, "claude-afterlife")
	marker := filepath.Join(dir, "snapshots")
	plistPath := filepath.Join(dir, label+".plist")
	writeExecutable(t, binary, "#!/bin/sh\necho \"$@\" >> "+shellQuote(marker)+"\n")
	plist := renderPlist(label, agentProgram(binary, plistPath, label, time.Second), nil, 10*time.Second, filepath.Join(dir, "error.log"))
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	t.Cleanup(func() { _ = exec.Command("launchctl", "bootout", target).Run() })
	if output, err := exec.Command("launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath).CombinedOutput(); err != nil {
		t.Fatalf("launchctl bootstrap: %v\n%s", err, output)
	}

	waitFor(t, 20*time.Second, "the agent to take a snapshot", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 45*time.Second, "the agent to remove itself", func() bool {
		_, statErr := os.Stat(plistPath)
		loaded := exec.Command("launchctl", "print", target).Run() == nil
		return statErr != nil && !loaded
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
