package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetectsTerminal(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		path []string
		want string
	}{
		{"nothing known", nil, nil, "print"},
		{"thinkterm on PATH", nil, []string{"wezterm", "thinkterm"}, "thinkterm"},
		{"ghostty", map[string]string{"TERM_PROGRAM": "ghostty"}, []string{"thinkterm"}, "ghostty"},
		{"iterm", map[string]string{"TERM_PROGRAM": "iTerm.app"}, nil, "iterm"},
		{"terminal", map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, nil, "terminal"},
		{"wezterm", map[string]string{"TERM_PROGRAM": "WezTerm", "WEZTERM_PANE": "3"}, nil, "wezterm"},
		{"kitty", map[string]string{"KITTY_WINDOW_ID": "1", "TERM_PROGRAM": "ghostty"}, nil, "kitty"},
		{"thinkterm by socket", map[string]string{"TERM_PROGRAM": "WezTerm", "WEZTERM_UNIX_SOCKET": "/Users/me/.local/share/thinkterm/gui-sock-1"}, nil, "thinkterm"},
		{"thinkterm by variable", map[string]string{"THINKTERM": "1"}, nil, "thinkterm"},
		{"tmux wins", map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TERM_PROGRAM": "ghostty"}, nil, "tmux"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			for key, value := range c.env {
				f.env[key] = value
			}
			for _, file := range c.path {
				f.paths[file] = "/bin/" + file
			}
			if got := f.app.detectTerminal(); got != c.want {
				t.Fatalf("detectTerminal() = %q, want %q", got, c.want)
			}
		})
	}
}

func terminalFixture(t *testing.T) *fixture {
	f := spawnFixture(t)
	f.paths["osascript"] = "/fake/bin/osascript"
	f.paths["kitten"] = "/fake/bin/kitten"
	return f
}

func (f *fixture) zshCommand(id string) string {
	return "/bin/zsh -l -i -c 'claude --resume " + id + "; exec /bin/zsh -l'"
}

func (f *fixture) typed(id string) string {
	return "cd " + f.work + " && claude --resume " + id
}

func TestGhosttyOpensAWindowThenTabsInIt(t *testing.T) {
	f := terminalFixture(t)
	f.runOut = "tab-group-1a2b"

	assertCode(t, f.run("restore", "-terminal", "ghostty", "-yes"), 0, f)

	want := [][]string{
		{"/fake/bin/osascript", "-e", ghosttyScript, "new", f.work, f.zshCommand(id1)},
		{"/fake/bin/osascript", "-e", ghosttyScript, "tab-group-1a2b", f.work, f.zshCommand(id2)},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls:\n%q\nwant:\n%q", f.calls, want)
	}
}

func TestGhosttyUsesTheFrontWindowWhenRunInsideIt(t *testing.T) {
	f := terminalFixture(t)
	f.env["TERM_PROGRAM"] = "ghostty"

	f.run("restore", "-yes")

	for _, call := range f.calls {
		if call[3] != "front" {
			t.Fatalf("destination %q, want front: %q", call[3], call)
		}
	}
	if len(f.calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(f.calls))
	}
}

func TestITermTypesTheCommandIntoNewTabs(t *testing.T) {
	f := terminalFixture(t)

	f.run("restore", "-terminal", "iterm", "-yes")

	want := [][]string{
		{"/fake/bin/osascript", "-e", itermScript, "new", f.typed(id1)},
		{"/fake/bin/osascript", "-e", itermScript, "42", f.typed(id2)},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls:\n%q\nwant:\n%q", f.calls, want)
	}
}

func TestAppleTerminalOpensAWindowPerSession(t *testing.T) {
	f := terminalFixture(t)

	f.run("restore", "-terminal", "terminal", "-yes")

	want := [][]string{
		{"/fake/bin/osascript", "-e", appleTerminalScript, f.typed(id1)},
		{"/fake/bin/osascript", "-e", appleTerminalScript, f.typed(id2)},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls:\n%q\nwant:\n%q", f.calls, want)
	}
}

func TestKittyOpensTabsInTheCurrentWindow(t *testing.T) {
	f := terminalFixture(t)
	f.env["KITTY_WINDOW_ID"] = "1"

	f.run("restore", "-yes")

	shell := f.shell(id1)
	want := append([]string{"/fake/bin/kitten", "@", "launch", "--cwd", f.work, "--type=tab", "--"}, shell...)
	if len(f.calls) != 2 || !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("calls:\n%q\nwant first:\n%q", f.calls, want)
	}
}

func TestKittyFromOutsideOpensAWindowThenTabs(t *testing.T) {
	f := terminalFixture(t)
	f.env["KITTY_LISTEN_ON"] = "unix:/tmp/kitty"

	f.run("restore", "-terminal", "kitty", "-yes")

	want := [][]string{
		append([]string{"/fake/bin/kitten", "@", "launch", "--cwd", f.work, "--type=os-window", "--"}, f.shell(id1)...),
		append([]string{"/fake/bin/kitten", "@", "launch", "--cwd", f.work, "--type=tab", "--match", "window_id:42", "--"}, f.shell(id2)...),
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls:\n%q\nwant:\n%q", f.calls, want)
	}
}

func TestKittyExplainsHowToEnableRemoteControl(t *testing.T) {
	f := terminalFixture(t)

	assertCode(t, f.run("restore", "-terminal", "kitty", "-yes"), 1, f)
	assertContains(t, f.stderr.String(), "listen_on")
	if len(f.calls) != 0 {
		t.Fatalf("ran %q without a way to reach kitty", f.calls)
	}

	f.env["KITTY_WINDOW_ID"] = "1"
	f.runErr = os.ErrPermission
	assertCode(t, f.run("restore", "-terminal", "kitty", "-yes"), 1, f)
	assertContains(t, f.stderr.String(), "allow_remote_control yes")
}

func TestAppleScriptTerminalsNeedMacOS(t *testing.T) {
	f := terminalFixture(t)
	f.app.GOOS = "linux"

	assertCode(t, f.run("restore", "-terminal", "ghostty", "-yes"), 1, f)

	assertContains(t, f.stderr.String(), "only be controlled on macOS")
	if len(f.calls) != 0 {
		t.Fatalf("ran %q", f.calls)
	}
}

func TestAppleScriptsCompile(t *testing.T) {
	osacompile, err := exec.LookPath("osacompile")
	if err != nil {
		t.Skip("osacompile is only available on macOS")
	}
	scripts := []struct {
		app    string
		script string
	}{
		{"/Applications/Ghostty.app", ghosttyScript},
		{"/Applications/iTerm.app", itermScript},
		{"/System/Applications/Utilities/Terminal.app", appleTerminalScript},
	}
	for _, s := range scripts {
		t.Run(filepath.Base(s.app), func(t *testing.T) {
			if _, err := os.Stat(s.app); err != nil {
				t.Skipf("%s is not installed", s.app)
			}
			output, err := exec.Command(osacompile, "-o", filepath.Join(t.TempDir(), "script.scpt"), "-e", s.script).CombinedOutput()
			if err != nil {
				t.Fatalf("osacompile: %v\n%s", err, output)
			}
		})
	}
}
