// claude-afterlife reopens the Claude Code sessions lost to a reboot or a terminal quitting.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

const name = "claude-afterlife"

// version is stamped by release builds with -ldflags "-X main.version=1.2.3".
var version = ""

type BootInfo struct {
	ID   string
	Time time.Time
}

// App holds everything that touches the outside world, so tests can replace it.
type App struct {
	ClaudeDir string
	StateDir  string
	HomeDir   string
	KeepBoots int
	GOOS      string

	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	StdinTTY  bool
	StdoutTTY bool

	Getenv     func(string) string
	Now        func() time.Time
	Boot       func() (BootInfo, error)
	Alive      func(pid int) bool
	LookPath   func(file string) (string, error)
	Run        func(argv []string) (string, error)
	Executable func() (string, error)
}

func main() {
	app, err := newApp()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
	os.Exit(app.Main(os.Args[1:]))
}

func newApp() (*App, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	app := &App{
		HomeDir:    home,
		KeepBoots:  20,
		GOOS:       runtime.GOOS,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		StdinTTY:   isTerminal(os.Stdin),
		StdoutTTY:  isTerminal(os.Stdout),
		Getenv:     os.Getenv,
		Now:        time.Now,
		Boot:       currentBoot,
		Alive:      pidAlive,
		LookPath:   exec.LookPath,
		Run:        runCommand,
		Executable: executablePath,
	}
	app.ClaudeDir = app.envPath("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	stateBase := app.envPath("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	app.StateDir = app.envPath("CLAUDE_AFTERLIFE_DIR", filepath.Join(stateBase, name))
	return app, nil
}

func (a *App) envPath(key, fallback string) string {
	value := a.Getenv(key)
	if value == "" {
		return fallback
	}
	if value == "~" {
		return a.HomeDir
	}
	if rest, ok := strings.CutPrefix(value, "~/"); ok {
		return filepath.Join(a.HomeDir, rest)
	}
	return value
}

const usage = `claude-afterlife reopens the Claude Code sessions lost to a reboot or a terminal quitting.

Usage:
  claude-afterlife <command> [flags]

Commands:
  snapshot    record the sessions that are running now (run this every minute)
  list        show recorded boots and the sessions seen during one of them
  restore     reopen the sessions lost since the last restore
  install     run snapshot every minute from a launchd agent (macOS)
  uninstall   remove that launchd agent
  version     print the version

Run 'claude-afterlife <command> -h' for the flags of a command.
`

func (a *App) Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Stderr, usage)
		return 2
	}
	commands := map[string]func([]string) int{
		"snapshot":  a.cmdSnapshot,
		"list":      a.cmdList,
		"restore":   a.cmdRestore,
		"install":   a.cmdInstall,
		"uninstall": a.cmdUninstall,
	}
	switch command := args[0]; command {
	case "version", "-version", "--version":
		fmt.Fprintf(a.Stdout, "%s %s\n", name, resolveVersion())
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(a.Stdout, usage)
		return 0
	default:
		run, ok := commands[command]
		if !ok {
			fmt.Fprintf(a.Stderr, "%s: unknown command %q\n\n%s", name, command, usage)
			return 2
		}
		return run(args[1:])
	}
}

// parse reports -1 when parsing succeeded, otherwise the exit code to return.
func (a *App) parse(set *flag.FlagSet, args []string) int {
	set.SetOutput(a.Stderr)
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if set.NArg() > 0 {
		fmt.Fprintf(a.Stderr, "%s %s: unexpected argument %q\n", name, set.Name(), set.Arg(0))
		return 2
	}
	return -1
}

func (a *App) fail(err error) int {
	fmt.Fprintf(a.Stderr, "%s: %v\n", name, err)
	return 1
}

func resolveVersion() string {
	if version != "" {
		return strings.TrimPrefix(version, "v")
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return strings.TrimPrefix(v, "v")
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) >= 12 {
			return "dev-" + setting.Value[:12]
		}
	}
	return "dev"
}

func executablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func runCommand(argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return "", errors.New(message)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}
