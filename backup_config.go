package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var destinationNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

type backupDestination struct {
	Remote    string    `json:"remote,omitempty"`
	Recipient string    `json:"recipient"`
	MACKey    string    `json:"mac_key"`
	CreatedAt time.Time `json:"created_at"`
}

type backupRoute struct {
	Pattern     string `json:"pattern"`
	Destination string `json:"destination"`
}

type backupConfig struct {
	MachineID         string                       `json:"machine_id"`
	Destinations      map[string]backupDestination `json:"destinations"`
	Routes            []backupRoute                `json:"routes"`
	ConfigDestination string                       `json:"config_destination,omitempty"`
}

func (a *App) backupDir() string {
	return filepath.Join(a.StateDir, "backup")
}

func (a *App) backupConfigPath() string {
	return filepath.Join(a.backupDir(), "config.json")
}

func (a *App) storeFor(name string) store {
	return store{dir: filepath.Join(a.backupDir(), name, "store")}
}

func (a *App) backupStatePath(name string) string {
	return filepath.Join(a.backupDir(), name, "state.json")
}

func (a *App) loadBackupConfig() (*backupConfig, error) {
	config := &backupConfig{Destinations: map[string]backupDestination{}}
	data, err := os.ReadFile(a.backupConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		config.MachineID = newMachineID()
		return config, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("reading %s: %w", a.backupConfigPath(), err)
	}
	if config.Destinations == nil {
		config.Destinations = map[string]backupDestination{}
	}
	if !machineIDPattern.MatchString(config.MachineID) {
		return nil, fmt.Errorf("%s has an invalid machine id", a.backupConfigPath())
	}
	return config, nil
}

func (a *App) saveBackupConfig(config *backupConfig) error {
	return writePrivate(a.backupConfigPath(), config)
}

func (config *backupConfig) destinationNames() []string {
	names := make([]string, 0, len(config.Destinations))
	for name := range config.Destinations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (a *App) expandHome(path string) string {
	switch {
	case path == "~":
		return a.HomeDir
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(a.HomeDir, path[2:])
	default:
		return path
	}
}

// globRegexp turns a path pattern into a regexp: * and ? stay within one folder,
// ** crosses folders, and a trailing /** also matches the folder itself.
func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "/**"):
			b.WriteString("(/.*)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func (a *App) normalizePattern(pattern string) (string, error) {
	expanded := a.expandHome(strings.TrimSpace(pattern))
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("route %q must be an absolute path or start with ~/", pattern)
	}
	if strings.HasSuffix(expanded, "/") && !strings.HasSuffix(expanded, "/**") {
		expanded = strings.TrimRight(expanded, "/")
	}
	if _, err := globRegexp(expanded); err != nil {
		return "", fmt.Errorf("invalid route %q: %w", pattern, err)
	}
	return expanded, nil
}

// destinationFor returns the destination of a project path, "" when no route
// matches. With several matching routes the longest pattern wins, so a more
// specific route can override a broader one.
func (a *App) destinationFor(config *backupConfig, projectPath string) string {
	if projectPath == "" {
		return ""
	}
	best, bestLength := "", -1
	for _, route := range config.Routes {
		pattern, err := a.normalizePattern(route.Pattern)
		if err != nil {
			continue
		}
		re, err := globRegexp(pattern)
		if err != nil || !re.MatchString(filepath.Clean(projectPath)) {
			continue
		}
		if len(pattern) > bestLength {
			best, bestLength = route.Destination, len(pattern)
		}
	}
	return best
}
