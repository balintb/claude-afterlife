package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

type spawnOptions struct {
	ClaudeCommand string
	NewWindow     bool
}

type terminal struct {
	binaries []string
	macOnly  bool
	spawn    func(a *App, binary string, sessions []Session, opts spawnOptions) int
}

var terminalNames = []string{"ghostty", "iterm", "terminal", "kitty", "thinkterm", "wezterm", "tmux", "print"}

var terminals = map[string]terminal{
	"ghostty":   {binaries: []string{"osascript"}, macOnly: true, spawn: (*App).spawnGhostty},
	"iterm":     {binaries: []string{"osascript"}, macOnly: true, spawn: (*App).spawnITerm},
	"terminal":  {binaries: []string{"osascript"}, macOnly: true, spawn: (*App).spawnAppleTerminal},
	"kitty":     {binaries: []string{"kitten", "kitty"}, spawn: (*App).spawnKitty},
	"thinkterm": {binaries: []string{"thinkterm"}, spawn: (*App).spawnWezTermCLI},
	"wezterm":   {binaries: []string{"wezterm"}, spawn: (*App).spawnWezTermCLI},
	"tmux":      {binaries: []string{"tmux"}, spawn: (*App).spawnTmux},
}

// detectTerminal picks the terminal restore was started from, falling back to one on PATH.
func (a *App) detectTerminal() string {
	switch {
	case a.Getenv("TMUX") != "":
		return "tmux"
	case a.Getenv("KITTY_WINDOW_ID") != "":
		return "kitty"
	case a.inThinkTerm():
		return "thinkterm"
	}
	switch a.Getenv("TERM_PROGRAM") {
	case "ghostty":
		return "ghostty"
	case "iTerm.app":
		return "iterm"
	case "Apple_Terminal":
		return "terminal"
	case "WezTerm":
		return "wezterm"
	}
	for _, candidate := range []string{"thinkterm", "wezterm"} {
		if _, err := a.LookPath(candidate); err == nil {
			return candidate
		}
	}
	return "print"
}

// inThinkTerm tells ThinkTerm apart from WezTerm, which it is forked from and
// shares its WEZTERM_* variables with.
func (a *App) inThinkTerm() bool {
	return a.Getenv("THINKTERM") != "" ||
		strings.EqualFold(a.Getenv("TERM_PROGRAM"), "thinkterm") ||
		strings.Contains(strings.ToLower(a.Getenv("WEZTERM_UNIX_SOCKET")), "thinkterm")
}

// resolveBinary follows symlinks because ThinkTerm looks for its helper binaries
// next to the path it was started from.
func (a *App) resolveBinary(names []string) (string, error) {
	for _, file := range names {
		path, err := a.LookPath(file)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved, nil
		}
		return path, nil
	}
	return "", errors.New(strings.Join(names, " or ") + " is not on PATH")
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

func shellQuote(value string) string {
	if shellSafe.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func resumeCommand(claudeCommand, sessionID string) string {
	return claudeCommand + " --resume " + shellQuote(sessionID)
}

// typedCommand is what gets typed into a fresh shell by terminals that can only
// send text to a new tab.
func typedCommand(session Session, claudeCommand string) string {
	return "cd " + shellQuote(session.Cwd) + " && " + resumeCommand(claudeCommand, session.ID)
}

// tabCommand runs Claude through an interactive login shell, so PATH and version
// managers are set up as usual, and leaves that shell open once Claude exits.
func (a *App) tabCommand(claudeCommand, sessionID string) []string {
	shell := a.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	script := fmt.Sprintf("%s; exec %s -l", resumeCommand(claudeCommand, sessionID), shellQuote(shell))
	return []string{shell, "-l", "-i", "-c", script}
}

func (a *App) reportFailure(session Session, err error) {
	fmt.Fprintf(a.Stderr, "  failed to open %s: %v\n", shortID(session.ID), err)
}

// spawnInWindows is the shared shape of every tabbed backend: inside the terminal
// the tabs join the current window; outside it the first session opens a new window
// and the rest join that one. open returns the id that addresses the new window.
func (a *App) spawnInWindows(sessions []Session, inside bool, opts spawnOptions, open func(session Session, inCurrent bool, anchor string) (string, error)) int {
	inCurrent := inside && !opts.NewWindow
	anchor := ""
	failures := 0
	for _, session := range sessions {
		id, err := open(session, inCurrent, anchor)
		if err != nil {
			failures++
			a.reportFailure(session, err)
			continue
		}
		if !inCurrent && anchor == "" && id != "" {
			anchor = id
		}
	}
	return failures
}

func (a *App) spawnWezTermCLI(binary string, sessions []Session, opts spawnOptions) int {
	return a.spawnInWindows(sessions, a.Getenv("WEZTERM_PANE") != "", opts, func(session Session, inCurrent bool, anchor string) (string, error) {
		argv := []string{binary, "cli", "spawn", "--cwd", session.Cwd}
		switch {
		case anchor != "":
			argv = append(argv, "--pane-id", anchor)
		case !inCurrent:
			argv = append(argv, "--new-window")
		}
		argv = append(append(argv, "--"), a.tabCommand(opts.ClaudeCommand, session.ID)...)
		return digitsOnly(a.Run(argv))
	})
}

func (a *App) spawnKitty(binary string, sessions []Session, opts spawnOptions) int {
	inside := a.Getenv("KITTY_WINDOW_ID") != ""
	if !inside && a.Getenv("KITTY_LISTEN_ON") == "" {
		fmt.Fprintln(a.Stderr, "  kitty can only be controlled from inside kitty, or with listen_on set in kitty.conf and KITTY_LISTEN_ON exported.")
		return len(sessions)
	}
	failures := a.spawnInWindows(sessions, inside, opts, func(session Session, inCurrent bool, anchor string) (string, error) {
		argv := []string{binary, "@", "launch", "--cwd", session.Cwd}
		switch {
		case anchor != "":
			argv = append(argv, "--type=tab", "--match", "window_id:"+anchor)
		case inCurrent:
			argv = append(argv, "--type=tab")
		default:
			argv = append(argv, "--type=os-window")
		}
		argv = append(append(argv, "--"), a.tabCommand(opts.ClaudeCommand, session.ID)...)
		return digitsOnly(a.Run(argv))
	})
	if failures > 0 {
		fmt.Fprintln(a.Stderr, "  kitty only accepts these commands with remote control on: add 'allow_remote_control yes' to kitty.conf and restart kitty.")
	}
	return failures
}

func (a *App) spawnTmux(binary string, sessions []Session, opts spawnOptions) int {
	failures := 0
	for _, session := range sessions {
		argv := append([]string{binary, "new-window", "-c", session.Cwd}, a.tabCommand(opts.ClaudeCommand, session.ID)...)
		if _, err := a.Run(argv); err != nil {
			failures++
			a.reportFailure(session, err)
		}
	}
	return failures
}

// ghosttyScript needs Ghostty 1.3 or newer, the first release with AppleScript support.
// Arguments: "new", "front" or a window id; the working directory; the command.
const ghosttyScript = `on run argv
	set {destination, workingDirectory, shellCommand} to argv
	tell application "Ghostty"
		activate
		set surfaceConfig to new surface configuration
		set initial working directory of surfaceConfig to workingDirectory
		set command of surfaceConfig to shellCommand
		if destination is "new" then
			return id of (new window with configuration surfaceConfig)
		else if destination is "front" then
			new tab in front window with configuration surfaceConfig
		else
			new tab in (first window whose id is destination) with configuration surfaceConfig
		end if
	end tell
	return ""
end run`

func (a *App) spawnGhostty(binary string, sessions []Session, opts spawnOptions) int {
	return a.spawnInWindows(sessions, a.Getenv("TERM_PROGRAM") == "ghostty", opts, func(session Session, inCurrent bool, anchor string) (string, error) {
		command := shellJoin(a.tabCommand(opts.ClaudeCommand, session.ID))
		return a.Run([]string{binary, "-e", ghosttyScript, destination(inCurrent, anchor), session.Cwd, command})
	})
}

// itermScript types the command into a new tab of the default profile.
// Arguments: "new", "front" or a window id; the command.
const itermScript = `on run argv
	set {destination, shellCommand} to argv
	tell application "iTerm2"
		activate
		if destination is "new" then
			set newWindow to (create window with default profile)
			tell current session of newWindow to write text shellCommand
			return (id of newWindow) as text
		end if
		if destination is "front" then
			set targetWindow to current window
		else
			set targetWindow to (first window whose id is (destination as integer))
		end if
		tell targetWindow to set newTab to (create tab with default profile)
		tell current session of newTab to write text shellCommand
	end tell
	return ""
end run`

func (a *App) spawnITerm(binary string, sessions []Session, opts spawnOptions) int {
	return a.spawnInWindows(sessions, a.Getenv("TERM_PROGRAM") == "iTerm.app", opts, func(session Session, inCurrent bool, anchor string) (string, error) {
		return a.Run([]string{binary, "-e", itermScript, destination(inCurrent, anchor), typedCommand(session, opts.ClaudeCommand)})
	})
}

// appleTerminalScript opens a window per session: Terminal.app has no scripting
// command for new tabs. When this call is what launches Terminal, the first
// session takes over the window Terminal opens at launch instead of leaving it empty.
const appleTerminalScript = `on run argv
	set wasRunning to application "Terminal" is running
	tell application "Terminal"
		activate
		if not wasRunning then
			repeat 50 times
				if (count of windows) > 0 then exit repeat
				delay 0.1
			end repeat
		end if
		if not wasRunning and (count of windows) > 0 then
			do script (item 1 of argv) in window 1
		else
			do script (item 1 of argv)
		end if
	end tell
	return ""
end run`

func (a *App) spawnAppleTerminal(binary string, sessions []Session, opts spawnOptions) int {
	failures := 0
	for _, session := range sessions {
		if _, err := a.Run([]string{binary, "-e", appleTerminalScript, typedCommand(session, opts.ClaudeCommand)}); err != nil {
			failures++
			a.reportFailure(session, err)
		}
	}
	return failures
}

func destination(inCurrent bool, anchor string) string {
	switch {
	case anchor != "":
		return anchor
	case inCurrent:
		return "front"
	default:
		return "new"
	}
}

func digitsOnly(output string, err error) (string, error) {
	if err != nil || !isDigits(output) {
		return "", err
	}
	return output, nil
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
