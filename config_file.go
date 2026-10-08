package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// duration reads a Go duration such as "10m" from the config file.
type duration struct {
	time.Duration
}

func (d *duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("%q is not a duration like 10m or 1h", text)
	}
	if parsed <= 0 {
		return fmt.Errorf("%q must be positive", text)
	}
	d.Duration = parsed
	return nil
}

type fileRoute struct {
	Path        string `toml:"path"`
	Destination string `toml:"destination"`
}

// fileConfig is config.toml. Every field is optional; flags and environment
// variables override it, and it overrides the built-in defaults.
type fileConfig struct {
	ClaudeDir string `toml:"claude_dir"`
	StateDir  string `toml:"state_dir"`
	Restore   struct {
		Terminal          string   `toml:"terminal"`
		ClaudeCommand     string   `toml:"claude_command"`
		Recent            duration `toml:"recent"`
		NewWindow         bool     `toml:"new_window"`
		IncludeBackground bool     `toml:"include_background"`
	} `toml:"restore"`
	Install struct {
		Interval duration `toml:"interval"`
	} `toml:"install"`
	Backup struct {
		Search   []string    `toml:"search"`
		Settings string      `toml:"settings"`
		Routes   []fileRoute `toml:"routes"`
	} `toml:"backup"`
}

func (a *App) configFilePath() string {
	if path := a.Getenv("CLAUDE_AFTERLIFE_CONFIG"); path != "" {
		return a.expandHome(path)
	}
	base := a.envPath("XDG_CONFIG_HOME", filepath.Join(a.HomeDir, ".config"))
	return filepath.Join(base, name, "config.toml")
}

// useConfigFile reads config.toml, if there is one, and applies the folders it
// sets unless the environment already chose them.
func (a *App) useConfigFile() error {
	a.ConfigPath = a.configFilePath()
	var config fileConfig
	metadata, err := toml.DecodeFile(a.ConfigPath, &config)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", a.fmtPath(a.ConfigPath), err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, key := range undecoded {
			keys[i] = key.String()
		}
		return fmt.Errorf("%s: unknown setting %s", a.fmtPath(a.ConfigPath), strings.Join(keys, ", "))
	}
	if terminal := config.Restore.Terminal; terminal != "" && terminal != "auto" && !containsString(terminalNames, terminal) {
		return fmt.Errorf("%s: restore.terminal %q is not one of auto, %s", a.fmtPath(a.ConfigPath), terminal, strings.Join(terminalNames, ", "))
	}
	for _, route := range config.Backup.Routes {
		if route.Path == "" || route.Destination == "" {
			return fmt.Errorf("%s: every [[backup.routes]] needs a path and a destination", a.fmtPath(a.ConfigPath))
		}
		if _, err := a.normalizePattern(route.Path); err != nil {
			return fmt.Errorf("%s: %w", a.fmtPath(a.ConfigPath), err)
		}
	}
	a.File = config
	if config.ClaudeDir != "" && a.Getenv("CLAUDE_CONFIG_DIR") == "" {
		a.ClaudeDir = filepath.Clean(a.expandHome(config.ClaudeDir))
	}
	if config.StateDir != "" && a.Getenv("CLAUDE_AFTERLIFE_DIR") == "" {
		a.StateDir = filepath.Clean(a.expandHome(config.StateDir))
	}
	return nil
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func firstSet(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (a *App) defaultTerminal() string {
	return firstSet(a.Getenv("CLAUDE_AFTERLIFE_TERMINAL"), a.File.Restore.Terminal, "auto")
}

func (a *App) defaultClaudeCommand() string {
	return firstSet(a.Getenv("CLAUDE_AFTERLIFE_CLAUDE"), a.File.Restore.ClaudeCommand, "claude")
}

func (a *App) defaultRecent() time.Duration {
	if a.File.Restore.Recent.Duration > 0 {
		return a.File.Restore.Recent.Duration
	}
	return defaultRecent
}

func (a *App) defaultInterval() time.Duration {
	if a.File.Install.Interval.Duration > 0 {
		return a.File.Install.Interval.Duration
	}
	return time.Minute
}

func (a *App) defaultSearch() []string {
	if len(a.File.Backup.Search) > 0 {
		return a.File.Backup.Search
	}
	return []string{a.HomeDir}
}

func (a *App) fileRoutes() []backupRoute {
	routes := make([]backupRoute, 0, len(a.File.Backup.Routes))
	for _, route := range a.File.Backup.Routes {
		routes = append(routes, backupRoute{Pattern: route.Path, Destination: route.Destination, FromFile: true})
	}
	return routes
}

const configTemplate = `# claude-afterlife configuration. Every setting is optional, and the values shown
# are the defaults. Flags and environment variables override this file.

# Where Claude Code keeps its data (CLAUDE_CONFIG_DIR overrides this).
# claude_dir = "~/.claude"

# Where claude-afterlife keeps snapshots and backups (CLAUDE_AFTERLIFE_DIR overrides this).
# state_dir = "~/.local/state/claude-afterlife"

[restore]
# terminal = "auto"              # auto, ghostty, iterm, terminal, kitty, thinkterm, wezterm, tmux, print
# claude_command = "claude"      # flags allowed, for example "claude --model opus"
# recent = "10m"                 # how close to the latest loss a session must have ended
# new_window = false
# include_background = false

[install]
# interval = "1m"                # how often the background job takes a snapshot

[backup]
# search = ["~"]                 # where restore and resume look for repositories by git remote
# settings = "personal"          # destination for Claude Code settings and skills, or "none"

# Routes decide which projects are backed up, and where. They add to the routes
# made with: claude-afterlife backup route add
# [[backup.routes]]
# path = "~/code/**"
# destination = "personal"
`

func (a *App) cmdConfig(args []string) int {
	if len(args) > 0 && args[0] == "init" {
		if len(args) > 1 {
			fmt.Fprintln(a.Stderr, "Usage: claude-afterlife config init")
			return 2
		}
		path := a.configFilePath()
		if _, err := os.Stat(path); err == nil {
			return a.fail(fmt.Errorf("%s already exists", a.fmtPath(path)))
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return a.fail(err)
		}
		if err := os.WriteFile(path, []byte(configTemplate), 0o644); err != nil {
			return a.fail(err)
		}
		fmt.Fprintf(a.Stdout, "Wrote %s. Uncomment the settings you want to change.\n", a.fmtPath(path))
		return 0
	}
	set := flag.NewFlagSet("config", flag.ContinueOnError)
	if code := a.parse(set, args); code >= 0 {
		return code
	}
	state := "not found, using the defaults; create one with: claude-afterlife config init"
	if _, err := os.Stat(a.ConfigPath); err == nil {
		state = "in use"
	}
	fmt.Fprintf(a.Stdout, "Config file: %s (%s)\n\n", a.fmtPath(a.ConfigPath), state)
	rows := [][2]string{
		{"claude_dir", a.fmtPath(a.ClaudeDir)},
		{"state_dir", a.fmtPath(a.StateDir)},
		{"restore.terminal", a.defaultTerminal()},
		{"restore.claude_command", a.defaultClaudeCommand()},
		{"restore.recent", a.defaultRecent().String()},
		{"restore.new_window", fmt.Sprint(a.File.Restore.NewWindow)},
		{"restore.include_background", fmt.Sprint(a.File.Restore.IncludeBackground)},
		{"install.interval", a.defaultInterval().String()},
		{"backup.search", strings.Join(a.defaultSearch(), ", ")},
	}
	for _, row := range rows {
		fmt.Fprintf(a.Stdout, "  %-28s %s\n", row[0], row[1])
	}
	if a.File.Backup.Settings != "" {
		fmt.Fprintf(a.Stdout, "  %-28s %s\n", "backup.settings", a.File.Backup.Settings)
	}
	routes := a.fileRoutes()
	sort.SliceStable(routes, func(i, j int) bool { return routes[i].Pattern < routes[j].Pattern })
	for _, route := range routes {
		fmt.Fprintf(a.Stdout, "  %-28s %s  ->  %s\n", "backup.routes", route.Pattern, route.Destination)
	}
	return 0
}
