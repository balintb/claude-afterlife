package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const agentLabel = "local." + name

// agentScript is what launchd runs. It takes a snapshot while the binary exists.
// Once the binary is gone, for example after `brew uninstall`, the agent deletes
// its own plist and unloads itself, because nothing else will. The binary has to
// be missing on two checks $4 seconds apart, which rides out the moment
// `brew upgrade` swaps Homebrew's opt link. $5 replaces launchctl in tests.
const agentScript = `if [ -x "$1" ]; then exec "$1" snapshot; fi
/bin/sleep "$4"
if [ -x "$1" ]; then exec "$1" snapshot; fi
/bin/rm -f "$2"
exec "${5:-/bin/launchctl}" bootout "gui/$(/usr/bin/id -u)/$3"`

func agentProgram(executable, plistPath, label string, recheck time.Duration) []string {
	return []string{"/bin/sh", "-c", agentScript, name + "-agent", executable, plistPath, label, strconv.Itoa(int(recheck.Seconds()))}
}

func (a *App) agentPlistPath() string {
	return filepath.Join(a.HomeDir, "Library", "LaunchAgents", agentLabel+".plist")
}

func (a *App) cmdInstall(args []string) int {
	set := flag.NewFlagSet("install", flag.ContinueOnError)
	interval := set.Duration("interval", time.Minute, "how often to take a snapshot")
	noLoad := set.Bool("no-load", false, "write the launchd agent but do not start it")
	if code := a.parse(set, args); code >= 0 {
		return code
	}
	if runtime.GOOS != "darwin" {
		fmt.Fprintf(a.Stderr, "install sets up a launchd agent, which only exists on macOS. Run '%s snapshot' every minute from cron or a systemd timer instead (see the README).\n", name)
		return 1
	}
	if *interval < 10*time.Second {
		return a.fail(errors.New("-interval must be at least 10s"))
	}
	executable, err := a.Executable()
	if err != nil {
		return a.fail(err)
	}
	executable = stableExecutable(executable)
	if strings.Contains(executable, string(filepath.Separator)+"go-build") {
		return a.fail(errors.New("run install from an installed binary, not from 'go run'"))
	}

	if err := os.MkdirAll(a.StateDir, 0o700); err != nil {
		return a.fail(err)
	}
	plistPath := a.agentPlistPath()
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return a.fail(err)
	}
	environment := map[string]string{"CLAUDE_AFTERLIFE_DIR": a.StateDir}
	if value := a.Getenv("CLAUDE_CONFIG_DIR"); value != "" {
		environment["CLAUDE_CONFIG_DIR"] = a.ClaudeDir
	}
	program := agentProgram(executable, plistPath, agentLabel, 10*time.Second)
	plist := renderPlist(agentLabel, program, environment, *interval, filepath.Join(a.StateDir, "error.log"))
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.Stdout, "Wrote %s, which runs %s\n", a.fmtPath(plistPath), a.fmtPath(executable))

	if *noLoad {
		fmt.Fprintf(a.Stdout, "Not started. Start it with: launchctl bootstrap gui/%d %s\n", os.Getuid(), shellQuote(plistPath))
		return 0
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_, _ = a.Run([]string{"launchctl", "bootout", domain + "/" + agentLabel})
	if _, err := a.Run([]string{"launchctl", "bootstrap", domain, plistPath}); err != nil {
		return a.fail(fmt.Errorf("launchctl bootstrap: %w", err))
	}
	fmt.Fprintf(a.Stdout, "Started %s: a snapshot every %s, stored in %s\n", agentLabel, *interval, a.fmtPath(a.StateDir))
	return 0
}

func (a *App) cmdUninstall(args []string) int {
	set := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := set.Bool("purge", false, "also delete the recorded snapshots")
	if code := a.parse(set, args); code >= 0 {
		return code
	}
	if runtime.GOOS == "darwin" {
		_, _ = a.Run([]string{"launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), agentLabel)})
		plistPath := a.agentPlistPath()
		if err := os.Remove(plistPath); err == nil {
			fmt.Fprintf(a.Stdout, "Removed %s\n", a.fmtPath(plistPath))
		} else if !errors.Is(err, os.ErrNotExist) {
			return a.fail(err)
		}
	}
	if *purge {
		clean := filepath.Clean(a.StateDir)
		if clean == "/" || clean == filepath.Clean(a.HomeDir) {
			return a.fail(fmt.Errorf("refusing to delete %s", clean))
		}
		if err := os.RemoveAll(clean); err != nil {
			return a.fail(err)
		}
		fmt.Fprintf(a.Stdout, "Removed %s\n", a.fmtPath(clean))
	}
	return 0
}

// stableExecutable maps a Homebrew Cellar path, which names the installed version
// and disappears on the next upgrade, to the opt link Homebrew keeps pointing at
// whichever version is current.
func stableExecutable(executable string) string {
	marker := string(filepath.Separator) + filepath.Join("Cellar", name) + string(filepath.Separator)
	prefix, rest, found := strings.Cut(executable, marker)
	if !found {
		return executable
	}
	_, inside, found := strings.Cut(rest, string(filepath.Separator))
	if !found {
		return executable
	}
	opt := filepath.Join(prefix, "opt", name, inside)
	if _, err := os.Stat(opt); err != nil {
		return executable
	}
	return opt
}

func renderPlist(label string, program []string, environment map[string]string, interval time.Duration, errorLog string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	fmt.Fprintf(&b, "  <key>Label</key>\n  <string>%s</string>\n", xmlEscape(label))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range program {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(arg))
	}
	b.WriteString("  </array>\n  <key>EnvironmentVariables</key>\n  <dict>\n")
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "    <key>%s</key>\n    <string>%s</string>\n", xmlEscape(key), xmlEscape(environment[key]))
	}
	b.WriteString("  </dict>\n")
	fmt.Fprintf(&b, "  <key>StartInterval</key>\n  <integer>%d</integer>\n", int(interval.Seconds()))
	b.WriteString(`  <key>RunAtLoad</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>LowPriorityIO</key>
  <true/>
  <key>Nice</key>
  <integer>10</integer>
  <key>StandardOutPath</key>
  <string>/dev/null</string>
`)
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key>\n  <string>%s</string>\n", xmlEscape(errorLog))
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

var xmlReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func xmlEscape(value string) string {
	return xmlReplacer.Replace(value)
}
