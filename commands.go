package main

import (
	"bufio"
	"flag"
	"fmt"
	"strings"
	"time"
)

const defaultRecent = 10 * time.Minute

func (a *App) cmdSnapshot(args []string) int {
	set := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	if code := a.parse(set, args); code >= 0 {
		return code
	}
	boot, live, err := a.record()
	if err != nil {
		return a.fail(err)
	}
	if a.StdoutTTY {
		fmt.Fprintf(a.Stdout, "%d running session(s) recorded for boot %s\n", live, shortID(boot.BootID))
	}
	return 0
}

func (a *App) cmdList(args []string) int {
	set := flag.NewFlagSet("list", flag.ContinueOnError)
	which := set.String("boot", "", "previous, current, or a boot id prefix (default: previous, else current)")
	recent := set.Duration("recent", defaultRecent, "how close to the latest loss a session must have ended to count as a candidate")
	if code := a.parse(set, args); code >= 0 {
		return code
	}
	current, err := a.Boot()
	if err != nil {
		return a.fail(err)
	}
	boots := a.allBoots()
	if len(boots) == 0 {
		fmt.Fprintf(a.Stdout, "No snapshots in %s yet. Run '%s snapshot' or set up the scheduled job.\n", a.fmtPath(a.bootsDir()), name)
		return 1
	}
	sel := selection{Recent: *recent}
	running := a.runningNow(current)
	candidatesOf := func(boot *Boot) []Session {
		if boot.BootID == current.ID {
			return a.endedGroup(boot, running, time.Time{}, sel).Sessions
		}
		return a.shutdownGroup(boot, sel, nil).Sessions
	}
	fmt.Fprintln(a.Stdout, "Boots, newest first:")
	hasPrevious := false
	for _, boot := range boots {
		label := ""
		if boot.BootID == current.ID {
			label = "current"
		} else {
			hasPrevious = true
		}
		fmt.Fprintf(a.Stdout, "  %s  %-7s  last snapshot %s  %d seen, %d restore candidate(s)\n",
			shortID(boot.BootID), label, fmtTime(boot.UpdatedAt), len(boot.Sessions), len(candidatesOf(boot)))
	}
	if *which == "" {
		*which = "current"
		if hasPrevious {
			*which = "previous"
		}
	}
	boot := selectBoot(boots, *which, current.ID)
	if boot == nil {
		fmt.Fprintf(a.Stderr, "No snapshot found for boot %q.\n", *which)
		return 1
	}
	chosen := map[string]bool{}
	for _, session := range candidatesOf(boot) {
		chosen[session.ID] = true
	}
	fmt.Fprintf(a.Stdout, "\nSessions seen during boot %s (+ = restore candidate):\n", shortID(boot.BootID))
	sessions := make([]Session, 0, len(boot.Sessions))
	for _, session := range boot.Sessions {
		sessions = append(sessions, session)
	}
	sortByStart(sessions)
	for _, session := range sessions {
		mark := " "
		if chosen[session.ID] {
			mark = "+"
		}
		fmt.Fprintf(a.Stdout, "  %s %s  last seen %s\n", mark, a.describe(session), fmtTime(session.LastSeen))
	}
	return 0
}

func (a *App) cmdRestore(args []string) int {
	set := flag.NewFlagSet("restore", flag.ContinueOnError)
	which := set.String("boot", "auto", "auto, previous, current, or a boot id prefix")
	terminalName := set.String("terminal", envOr(a.Getenv("CLAUDE_AFTERLIFE_TERMINAL"), "auto"), "auto, "+strings.Join(terminalNames, ", "))
	newWindow := set.Bool("new-window", false, "open the tabs in a new window, even when run inside one")
	recent := set.Duration("recent", defaultRecent, "how close to the latest loss a session must have ended to be reopened")
	all := set.Bool("all", false, "every session that ended, however long ago")
	includeBackground := set.Bool("include-background", false, "also reopen non-interactive sessions")
	claudeCommand := set.String("claude-command", envOr(a.Getenv("CLAUDE_AFTERLIFE_CLAUDE"), "claude"), "command that starts Claude Code, flags allowed")
	dryRun := set.Bool("dry-run", false, "show what would be reopened, open nothing")
	var yes bool
	set.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	set.BoolVar(&yes, "y", false, "short for -yes")
	if code := a.parse(set, args); code >= 0 {
		return code
	}

	if *terminalName == "auto" {
		*terminalName = a.detectTerminal()
	}
	backend, known := terminals[*terminalName]
	if !known && *terminalName != "print" {
		fmt.Fprintf(a.Stderr, "Unknown terminal %q. Choose one of: auto, %s.\n", *terminalName, strings.Join(terminalNames, ", "))
		return 2
	}
	if backend.macOnly && a.GOOS != "darwin" {
		fmt.Fprintf(a.Stderr, "%s can only be controlled on macOS. Use -terminal print and run the commands yourself.\n", *terminalName)
		return 1
	}
	current, err := a.Boot()
	if err != nil {
		return a.fail(err)
	}
	running := a.runningNow(current)
	sel := selection{Recent: *recent, All: *all, IncludeBackground: *includeBackground}
	groups, err := a.restoreGroups(*which, sel, current, running)
	if err != nil {
		return a.fail(err)
	}

	info := a.Stdout
	if *terminalName == "print" {
		info = a.Stderr
	}
	var todo []Session
	for _, group := range groups {
		fmt.Fprintln(info, group.Title)
		for _, session := range group.Sessions {
			status := a.restoreStatus(session, running)
			fmt.Fprintf(info, "  %-18s  %s\n", status, a.describe(session))
			if status == statusReopen {
				todo = append(todo, session)
			}
		}
	}
	if len(todo) == 0 {
		fmt.Fprintln(info, "Nothing to reopen.")
		return 0
	}

	if *terminalName == "print" {
		for _, session := range todo {
			fmt.Fprintln(a.Stdout, typedCommand(session, *claudeCommand))
		}
		return 0
	}
	if *dryRun {
		fmt.Fprintf(a.Stdout, "Dry run: would reopen %d session(s) in %s.\n", len(todo), *terminalName)
		return 0
	}
	binary, err := a.resolveBinary(backend.binaries)
	if err != nil {
		return a.fail(err)
	}
	if !yes {
		if !a.StdinTTY {
			fmt.Fprintln(a.Stderr, "Not opening terminals without confirmation. Run restore in a terminal, or pass -yes.")
			return 1
		}
		if !a.confirm(fmt.Sprintf("Reopen %d session(s) in %s? [Y/n] ", len(todo), *terminalName)) {
			fmt.Fprintln(a.Stdout, "Nothing reopened.")
			return 1
		}
	}
	failures := backend.spawn(a, binary, todo, spawnOptions{ClaudeCommand: *claudeCommand, NewWindow: *newWindow})
	opened := len(todo) - failures
	fmt.Fprintf(a.Stdout, "Reopened %d of %d session(s) in %s.\n", opened, len(todo), *terminalName)
	if opened > 0 {
		if err := a.markRestored(current.ID, a.Now()); err != nil {
			fmt.Fprintf(a.Stderr, "%s: could not record the restore: %v\n", name, err)
		}
	}
	if failures > 0 {
		return 1
	}
	return 0
}

func (a *App) confirm(question string) bool {
	answer, ok := a.prompt(question)
	if !ok {
		return false
	}
	switch strings.ToLower(answer) {
	case "", "y", "yes":
		return true
	default:
		return false
	}
}

// prompt asks a question and reads one line. All prompts share one reader, so
// answers typed ahead are not lost between questions.
func (a *App) prompt(question string) (string, bool) {
	fmt.Fprint(a.Stdout, question)
	if a.lines == nil {
		a.lines = bufio.NewReader(a.Stdin)
	}
	answer, err := a.lines.ReadString('\n')
	if err != nil && answer == "" {
		return "", false
	}
	return strings.TrimSpace(answer), true
}

func envOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func shortID(id string) string {
	if id == "" {
		return "unknown"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func (a *App) fmtPath(path string) string {
	if path == a.HomeDir {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, a.HomeDir+"/"); ok {
		return "~/" + rest
	}
	return path
}

func (a *App) describe(session Session) string {
	label := session.Name
	if label == "" {
		label = "-"
	}
	return fmt.Sprintf("%s  started %s  %s  %s", shortID(session.ID), fmtTime(session.StartedAt), label, a.fmtPath(session.Cwd))
}
