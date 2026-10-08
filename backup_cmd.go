package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const backupUsage = `claude-afterlife backup saves your Claude Code sessions to an encrypted git
repository and restores them on a new or wiped machine. Code and credentials are
not included.

Usage:
  claude-afterlife backup init <name> <git-remote>   create a destination: a new, empty repository for the
                                                     backup (not your code's) and the key that opens it
  claude-afterlife backup route add <path> <name>    back up projects under <path> (~/code/**) to <name>
  claude-afterlife backup route remove <path>
  claude-afterlife backup route config <name>|none   where Claude Code settings and skills go, or nowhere
  claude-afterlife backup status                     destinations, routes, projects not backed up
  claude-afterlife backup run [<name>...]            back up now and push
  claude-afterlife backup verify [-remote] [<name>]  rebuild everything and compare with ~/.claude
  claude-afterlife backup restore -from <git-remote> restore on this machine

Before wiping a laptop: init, route add, run, then verify -remote with the private
key pasted from your password manager. Only wipe once it says it is safe.
`

const alphaNotice = `claude-afterlife backup is ALPHA: it is new and has had little real-world use.
Keep another backup of ~/.claude at hand, and only rely on this one after
"claude-afterlife backup verify -remote" says it is safe. You use it at your own risk.`

// acceptAlpha asks once per machine for consent to use an alpha feature, and
// records it in config, which the caller saves. Without a terminal it needs
// -accept-alpha or CLAUDE_AFTERLIFE_ACCEPT_ALPHA=1, and never assumes yes.
func (a *App) acceptAlpha(config *backupConfig, accepted bool) bool {
	if !config.AlphaAccepted.IsZero() {
		return true
	}
	fmt.Fprintln(a.Stdout, alphaNotice)
	if !accepted && a.Getenv("CLAUDE_AFTERLIFE_ACCEPT_ALPHA") != "1" {
		if !a.StdinTTY {
			fmt.Fprintln(a.Stderr, "Stopped: run this in a terminal to accept, or pass -accept-alpha.")
			return false
		}
		answer, ok := a.prompt("Continue at your own risk? [y/N] ")
		if !ok || (strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes") {
			fmt.Fprintln(a.Stdout, "Stopped. Nothing was changed.")
			return false
		}
	}
	config.AlphaAccepted = a.Now()
	fmt.Fprintln(a.Stdout)
	return true
}

// requireAlphaAccepted asks for consent and saves it right away, for commands
// that do not otherwise save the backup configuration. It returns -1 to go on.
func (a *App) requireAlphaAccepted(accepted bool) int {
	config, err := a.loadBackupConfig()
	if err != nil {
		return a.fail(err)
	}
	if !config.AlphaAccepted.IsZero() {
		return -1
	}
	if !a.acceptAlpha(config, accepted) {
		return 1
	}
	if err := a.saveBackupConfig(config); err != nil {
		return a.fail(err)
	}
	return -1
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// parseInterspersed allows flags anywhere among the positional arguments.
func (a *App) parseInterspersed(set *flag.FlagSet, args []string) ([]string, int) {
	set.SetOutput(a.Stderr)
	var positional []string
	for {
		if err := set.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, 0
			}
			return nil, 2
		}
		rest := set.Args()
		if len(rest) == 0 {
			return positional, -1
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func (a *App) cmdBackup(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Stderr, backupUsage)
		return 2
	}
	switch args[0] {
	case "init":
		return a.cmdBackupInit(args[1:])
	case "route":
		return a.cmdBackupRoute(args[1:])
	case "status":
		return a.cmdBackupStatus(args[1:])
	case "run":
		return a.cmdBackupRun(args[1:])
	case "verify":
		return a.cmdBackupVerify(args[1:])
	case "restore":
		return a.cmdBackupRestore(args[1:])
	case "help", "-h", "-help", "--help":
		fmt.Fprint(a.Stdout, backupUsage)
		return 0
	default:
		fmt.Fprintf(a.Stderr, "%s backup: unknown command %q\n\n%s", name, args[0], backupUsage)
		return 2
	}
}

func (a *App) cmdBackupInit(args []string) int {
	set := flag.NewFlagSet("backup init", flag.ContinueOnError)
	confirmed := set.Bool("confirm-key-saved", false, "do not ask: you will save the printed private key in your password manager")
	acceptAlpha := set.Bool("accept-alpha", false, "do not ask: you accept that backup is alpha and use it at your own risk")
	positional, code := a.parseInterspersed(set, args)
	if code >= 0 {
		return code
	}
	if len(positional) < 1 || len(positional) > 2 {
		fmt.Fprintln(a.Stderr, "Usage: claude-afterlife backup init <name> <git-remote>")
		return 2
	}
	destName, remote := positional[0], ""
	if len(positional) == 2 {
		remote = positional[1]
	}
	if !destinationNamePattern.MatchString(destName) || destName == noConfigDestination {
		return a.fail(fmt.Errorf("destination names use lowercase letters, digits, - and _, like personal or work"))
	}
	config, err := a.loadBackupConfig()
	if err != nil {
		return a.fail(err)
	}
	if _, exists := config.Destinations[destName]; exists {
		return a.fail(fmt.Errorf("a destination named %q already exists", destName))
	}
	if !a.acceptAlpha(config, *acceptAlpha) {
		return 1
	}
	st := a.storeFor(destName)
	destDir := filepath.Dir(st.dir)
	if _, err := os.Stat(destDir); err == nil {
		return a.fail(fmt.Errorf("%s already exists; remove it or pick another name", a.fmtPath(destDir)))
	}
	secret, recipient, err := newBackupKey()
	if err != nil {
		return a.fail(err)
	}
	macKey, err := macKeyFor(secret)
	if err != nil {
		return a.fail(err)
	}
	discard := func() { os.RemoveAll(destDir) }
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return a.fail(err)
	}
	if err := a.initStoreRepo(st.dir, remote); err != nil {
		discard()
		return a.fail(fmt.Errorf("creating the backup repository: %w", err))
	}
	if err := st.writeMeta(storeMeta{Format: backupFormat, Kind: storeKind, Recipients: []string{recipient}, CreatedAt: a.Now()}); err != nil {
		discard()
		return a.fail(err)
	}

	where := "kept on this machine only"
	if remote != "" {
		where = "pushes to " + remote
	}
	fmt.Fprintf(a.Stdout, "Created backup destination %q (%s).\n\n", destName, where)
	fmt.Fprintf(a.Stdout, "Private key, needed to restore this backup. It is shown only now:\n\n    %s\n\n", secret)
	fmt.Fprintln(a.Stdout, "Save it in your password manager. Without it this backup can never be opened again.")
	if !*confirmed {
		if !a.StdinTTY {
			discard()
			fmt.Fprintln(a.Stderr, "\nNothing was created: run this in a terminal to confirm the key is saved, or pass -confirm-key-saved.")
			return 1
		}
		answer, ok := a.prompt("Type saved once the key is in your password manager: ")
		if !ok || strings.ToLower(answer) != "saved" {
			discard()
			fmt.Fprintln(a.Stdout, "Not confirmed, so nothing was created and that key will never be used.")
			return 1
		}
	}
	config.Destinations[destName] = backupDestination{Remote: remote, Recipient: recipient, MACKey: hex.EncodeToString(macKey), CreatedAt: a.Now()}
	if config.ConfigDestination == "" {
		config.ConfigDestination = destName
	}
	if err := a.saveBackupConfig(config); err != nil {
		discard()
		return a.fail(err)
	}
	if a.settingsDestination(config) == destName {
		fmt.Fprintf(a.Stdout, "\nClaude Code settings and skills go to %q (change with: %s backup route config <name>, or none).\n", destName, name)
	}
	fmt.Fprintf(a.Stdout, "Next, choose which projects to back up, for example:\n  %s backup route add '~/code/**' %s\nthen run: %s backup run\n", name, destName, name)
	return 0
}

func (a *App) cmdBackupRoute(args []string) int {
	config, err := a.loadBackupConfig()
	if err != nil {
		return a.fail(err)
	}
	if len(args) == 0 || args[0] == "list" {
		a.printRoutes(config)
		return 0
	}
	switch {
	case args[0] == "add" && len(args) == 3:
		pattern, dest := args[1], args[2]
		if _, ok := config.Destinations[dest]; !ok {
			return a.fail(fmt.Errorf("no backup destination named %q", dest))
		}
		normalized, err := a.normalizePattern(pattern)
		if err != nil {
			return a.fail(err)
		}
		config.Routes = slices.DeleteFunc(config.Routes, func(route backupRoute) bool {
			existing, _ := a.normalizePattern(route.Pattern)
			return existing == normalized
		})
		config.Routes = append(config.Routes, backupRoute{Pattern: pattern, Destination: dest})
		if err := a.saveBackupConfig(config); err != nil {
			return a.fail(err)
		}
		matched := 0
		for _, folder := range a.projectFolders() {
			if a.destinationFor(config, folder.Path) == dest {
				matched++
			}
		}
		fmt.Fprintf(a.Stdout, "Projects under %s go to %q (%d project folders match now).\n", pattern, dest, matched)
		return 0
	case args[0] == "remove" && len(args) == 2:
		normalized, err := a.normalizePattern(args[1])
		if err != nil {
			return a.fail(err)
		}
		before := len(config.Routes)
		config.Routes = slices.DeleteFunc(config.Routes, func(route backupRoute) bool {
			existing, _ := a.normalizePattern(route.Pattern)
			return existing == normalized
		})
		if len(config.Routes) == before {
			for _, route := range a.fileRoutes() {
				if existing, _ := a.normalizePattern(route.Pattern); existing == normalized {
					return a.fail(fmt.Errorf("the route for %s is set in %s; remove it there", args[1], a.fmtPath(a.ConfigPath)))
				}
			}
			return a.fail(fmt.Errorf("no route for %s", args[1]))
		}
		if err := a.saveBackupConfig(config); err != nil {
			return a.fail(err)
		}
		fmt.Fprintf(a.Stdout, "Removed the route for %s. Sessions already backed up stay in the backup.\n", args[1])
		return 0
	case args[0] == "config" && len(args) == 2:
		if a.File.Backup.Settings != "" {
			return a.fail(fmt.Errorf("backup.settings is set in %s; change it there", a.fmtPath(a.ConfigPath)))
		}
		if _, ok := config.Destinations[args[1]]; !ok && args[1] != noConfigDestination {
			return a.fail(fmt.Errorf("no backup destination named %q (or use none)", args[1]))
		}
		config.ConfigDestination = args[1]
		if err := a.saveBackupConfig(config); err != nil {
			return a.fail(err)
		}
		if args[1] == noConfigDestination {
			fmt.Fprintln(a.Stdout, "Claude Code settings and skills are not backed up.")
		} else {
			fmt.Fprintf(a.Stdout, "Claude Code settings and skills go to %q.\n", args[1])
		}
		return 0
	default:
		fmt.Fprintln(a.Stderr, "Usage: claude-afterlife backup route add <path> <name> | remove <path> | config <name> | list")
		return 2
	}
}

func (a *App) printRoutes(config *backupConfig) {
	routes := a.allRoutes(config)
	if len(routes) == 0 {
		fmt.Fprintln(a.Stdout, "No routes: no projects are backed up yet.")
	} else {
		fmt.Fprintln(a.Stdout, "Routes:")
		for _, route := range routes {
			source := ""
			if route.FromFile {
				source = "  (config file)"
			}
			if _, ok := config.Destinations[route.Destination]; !ok {
				source += fmt.Sprintf("  (no destination %q yet: create it with backup init)", route.Destination)
			}
			fmt.Fprintf(a.Stdout, "  %s  ->  %s%s\n", route.Pattern, route.Destination, source)
		}
	}
	switch destination := a.settingsDestination(config); destination {
	case "", noConfigDestination:
		fmt.Fprintln(a.Stdout, "Claude Code settings and skills  ->  not backed up")
	default:
		fmt.Fprintf(a.Stdout, "Claude Code settings and skills  ->  %s\n", destination)
	}
}

func (a *App) cmdBackupStatus(args []string) int {
	set := flag.NewFlagSet("backup status", flag.ContinueOnError)
	if code := a.parse(set, args); code >= 0 {
		return code
	}
	config, err := a.loadBackupConfig()
	if err != nil {
		return a.fail(err)
	}
	if len(config.Destinations) == 0 {
		fmt.Fprintf(a.Stdout, "No backup destinations yet. Create one with: %s backup init <name> <git-remote>\n", name)
		if len(a.fileRoutes()) > 0 {
			a.printRoutes(config)
		}
		return 0
	}
	fmt.Fprintln(a.Stdout, "Destinations:")
	for _, destName := range config.destinationNames() {
		dest := config.Destinations[destName]
		remote := dest.Remote
		if remote == "" {
			remote = "no remote"
		}
		var state backupIndex
		last := "never backed up"
		if readJSON(a.backupStatePath(destName), &state) {
			sessions := 0
			for _, entry := range state.Files {
				if isSessionPath(entry.Path) {
					sessions++
				}
			}
			last = fmt.Sprintf("last backup %s, %d sessions in %d projects", fmtTime(state.UpdatedAt), sessions, len(state.Projects))
		}
		fmt.Fprintf(a.Stdout, "  %s  %s  %s\n", destName, remote, last)
	}
	a.printRoutes(config)
	a.printUnrouted(config)
	return 0
}

func (a *App) printUnrouted(config *backupConfig) {
	var unrouted, unknown []string
	for _, folder := range a.projectFolders() {
		sessions := countSessions(filepath.Join(a.projectsRoot(), folder.Slug))
		switch {
		case folder.Path == "":
			unknown = append(unknown, fmt.Sprintf("  %s  (%d sessions)", folder.Slug, sessions))
		case a.destinationFor(config, folder.Path) == "":
			unrouted = append(unrouted, fmt.Sprintf("  %s  (%d sessions)", a.fmtPath(folder.Path), sessions))
		}
	}
	if len(unrouted) > 0 {
		fmt.Fprintln(a.Stdout, "Not backed up (no route):")
		fmt.Fprintln(a.Stdout, strings.Join(unrouted, "\n"))
	}
	if len(unknown) > 0 {
		fmt.Fprintln(a.Stdout, "Not backed up (the project's folder could not be determined):")
		fmt.Fprintln(a.Stdout, strings.Join(unknown, "\n"))
	}
}

func countSessions(dir string) int {
	entries, _ := os.ReadDir(dir)
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".jsonl" {
			count++
		}
	}
	return count
}

func (a *App) cmdBackupRun(args []string) int {
	set := flag.NewFlagSet("backup run", flag.ContinueOnError)
	positional, code := a.parseInterspersed(set, args)
	if code >= 0 {
		return code
	}
	config, err := a.loadBackupConfig()
	if err != nil {
		return a.fail(err)
	}
	names := positional
	if len(names) == 0 {
		names = config.destinationNames()
	}
	if len(names) == 0 {
		return a.fail(fmt.Errorf("no backup destinations yet; create one with: %s backup init <name> <git-remote>", name))
	}
	failed := false
	var dirty, unpushed []string
	for _, destName := range names {
		report, err := a.runBackup(config, destName)
		if err != nil {
			fmt.Fprintf(a.Stderr, "%s: %v\n", destName, err)
			failed = true
			continue
		}
		a.printRunReport(config, report)
		if report.PushError != "" {
			failed = true
		}
		dirty = appendUnique(dirty, report.Dirty...)
		unpushed = appendUnique(unpushed, report.Unpushed...)
	}
	a.printUnrouted(config)
	if len(dirty) > 0 || len(unpushed) > 0 {
		fmt.Fprintln(a.Stdout, "Code is not part of the backup. Push it before wiping:")
		for _, repo := range dirty {
			fmt.Fprintf(a.Stdout, "  %s  uncommitted changes\n", a.fmtPath(repo))
		}
		for _, branch := range unpushed {
			fmt.Fprintf(a.Stdout, "  %s\n", a.fmtPath(branch))
		}
	}
	if failed {
		return 1
	}
	return 0
}

func (a *App) printRunReport(config *backupConfig, report runReport) {
	fmt.Fprintf(a.Stdout, "%s: %d sessions in %d projects. Uploaded %s in %d objects (%d new files, %d grown, %d rewritten)",
		report.Destination, report.Sessions, report.Projects, humanBytes(report.NewBytes), report.Objects,
		report.NewFiles, report.Updated, len(report.Rewritten))
	if report.Archived > 0 {
		fmt.Fprintf(a.Stdout, "; %d no longer on disk, kept in the backup", report.Archived)
	}
	fmt.Fprintln(a.Stdout, ".")
	if report.Reset {
		fmt.Fprintln(a.Stdout, "  The local backup state did not match the store, so everything was uploaded again.")
	}
	switch {
	case report.Pushed:
		fmt.Fprintf(a.Stdout, "  Pushed to %s.\n", config.Destinations[report.Destination].Remote)
	case report.PushError != "":
		fmt.Fprintf(a.Stdout, "  Push failed: %s\n  The backup is saved in %s; run backup run again to retry the push.\n", report.PushError, a.fmtPath(a.storeFor(report.Destination).dir))
	default:
		fmt.Fprintln(a.Stdout, "  No remote: this backup only exists on this machine.")
	}
}

func appendUnique(list []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(list, value) {
			list = append(list, value)
		}
	}
	return list
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func (a *App) resolveDestination(config *backupConfig, positional []string) (string, error) {
	if len(positional) > 1 {
		return "", errors.New("name one destination")
	}
	if len(positional) == 1 {
		if _, ok := config.Destinations[positional[0]]; !ok {
			return "", fmt.Errorf("no backup destination named %q", positional[0])
		}
		return positional[0], nil
	}
	names := config.destinationNames()
	switch len(names) {
	case 0:
		return "", errors.New("no backup destinations yet")
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("name the destination to verify: %s", strings.Join(names, ", "))
	}
}

func (a *App) readBackupKey(identity string) (backupKey, error) {
	var text string
	switch {
	case identity == "-":
		data, err := io.ReadAll(a.Stdin)
		if err != nil {
			return backupKey{}, err
		}
		text = string(data)
	case identity != "":
		data, err := os.ReadFile(a.expandHome(identity))
		if errors.Is(err, os.ErrNotExist) {
			return backupKey{}, fmt.Errorf("there is no private key file at %s. Point -identity at the file where you saved the key backup init printed, or leave out -identity to paste the key", a.fmtPath(a.expandHome(identity)))
		}
		if err != nil {
			return backupKey{}, err
		}
		text = string(data)
	case a.StdinTTY:
		secret, err := a.ReadSecret("Private key (AGE-SECRET-KEY-1...): ")
		if err != nil {
			return backupKey{}, err
		}
		text = secret
	default:
		return backupKey{}, errors.New("pass -identity <file>, or -identity - to read the private key from standard input")
	}
	return parseBackupKey(text)
}

func (a *App) cmdBackupVerify(args []string) int {
	set := flag.NewFlagSet("backup verify", flag.ContinueOnError)
	remote := set.Bool("remote", false, "verify a fresh clone of the remote instead of the local store")
	identity := set.String("identity", "", "file with the private key, or - for standard input (default: ask)")
	positional, code := a.parseInterspersed(set, args)
	if code >= 0 {
		return code
	}
	config, err := a.loadBackupConfig()
	if err != nil {
		return a.fail(err)
	}
	destName, err := a.resolveDestination(config, positional)
	if err != nil {
		return a.fail(err)
	}
	dest := config.Destinations[destName]
	key, err := a.readBackupKey(*identity)
	if err != nil {
		return a.fail(err)
	}
	if key.recipient != dest.Recipient {
		return a.fail(fmt.Errorf("this private key belongs to a different backup than %q", destName))
	}
	st, where := a.storeFor(destName), "the local store"
	if *remote {
		if dest.Remote == "" {
			return a.fail(fmt.Errorf("%q has no remote", destName))
		}
		cloned, cleanup, err := a.cloneStore(dest.Remote)
		if err != nil {
			return a.fail(fmt.Errorf("cloning %s: %w", dest.Remote, err))
		}
		defer cleanup()
		st, where = cloned, dest.Remote
	}
	report, err := a.verifyBackup(config, destName, st, key)
	if err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.Stdout, "Checked %d files (%d sessions) in %s: %d verified", report.Files, report.Sessions, where, report.Verified)
	if report.Archived > 0 {
		fmt.Fprintf(a.Stdout, ", including %d no longer on disk", report.Archived)
	}
	fmt.Fprintln(a.Stdout, ".")
	printLimited(a.Stdout, "Damaged:", report.Problems)
	printLimited(a.Stdout, "Not backed up yet:", report.Pending)
	switch {
	case len(report.Problems) > 0:
		fmt.Fprintln(a.Stdout, "The backup is damaged. Do not wipe; run backup run and verify again.")
		return 1
	case len(report.Pending) > 0:
		fmt.Fprintf(a.Stdout, "Not everything is backed up yet. End your Claude Code sessions, run %s backup run, then verify again.\n", name)
		return 1
	case !*remote:
		fmt.Fprintf(a.Stdout, "The local store is complete. Before wiping, check the copy on the remote too: %s backup verify -remote\n", name)
		return 0
	default:
		fmt.Fprintln(a.Stdout, "Everything is backed up and verified on the remote. Safe to wipe as far as these Claude Code sessions go; code and credentials are not in the backup.")
		return 0
	}
}

func printLimited(w io.Writer, heading string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, heading)
	for i, line := range lines {
		if i == 20 {
			fmt.Fprintf(w, "  ... and %d more\n", len(lines)-20)
			break
		}
		fmt.Fprintf(w, "  %s\n", line)
	}
}

func (a *App) cmdBackupRestore(args []string) int {
	set := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	var opts restoreOptions
	var maps, search stringList
	set.StringVar(&opts.From, "from", "", "git remote (or local folder) of the backup")
	set.StringVar(&opts.Identity, "identity", "", "file with the private key, or - for standard input (default: ask)")
	set.StringVar(&opts.Machine, "machine", "", "restore this machine's backup (id prefix); default: the most recent")
	set.Var(&maps, "map", "/old/path=/new/path: where a project's code lives now (repeatable)")
	set.Var(&search, "search", "folder to look for repositories in (repeatable; default: your home folder)")
	set.BoolVar(&opts.DryRun, "dry-run", false, "show the plan and write nothing")
	set.BoolVar(&opts.Yes, "yes", false, "do not ask for confirmation")
	set.BoolVar(&opts.KeepCleanup, "keep-cleanup", false, "do not raise cleanupPeriodDays")
	acceptAlpha := set.Bool("accept-alpha", false, "do not ask: you accept that backup is alpha and use it at your own risk")
	positional, code := a.parseInterspersed(set, args)
	if code >= 0 {
		return code
	}
	if len(positional) > 0 || opts.From == "" {
		fmt.Fprintln(a.Stderr, "Usage: claude-afterlife backup restore -from <git-remote> [-map /old=/new] [-search <folder>] [-dry-run]")
		return 2
	}
	opts.Maps, opts.Search = maps, search
	if code := a.requireAlphaAccepted(*acceptAlpha); code >= 0 {
		return code
	}
	st, cleanup, err := a.openBackupSource(opts.From)
	if err != nil {
		return a.fail(err)
	}
	defer cleanup()
	meta, err := st.readMeta()
	if err != nil {
		return a.fail(err)
	}
	key, err := a.readBackupKey(opts.Identity)
	if err != nil {
		return a.fail(err)
	}
	if !slices.Contains(meta.Recipients, key.recipient) {
		return a.fail(errWrongKey)
	}
	index, err := a.chooseIndex(st, key, opts.Machine)
	if err != nil {
		return a.fail(err)
	}
	mappings, err := a.planMappings(index, opts)
	if err != nil {
		return a.fail(err)
	}
	if a.StdinTTY && !opts.Yes {
		a.askForFolders(mappings)
	}
	plan, err := a.buildRestorePlan(st, key, index, mappings, opts)
	if err != nil {
		return a.fail(err)
	}
	a.printRestorePlan(plan)
	if opts.DryRun {
		fmt.Fprintln(a.Stdout, "Dry run: nothing was written.")
		return 0
	}
	if !opts.Yes {
		if !a.StdinTTY {
			fmt.Fprintln(a.Stderr, "Not restoring without confirmation. Run it in a terminal, or pass -yes.")
			return 1
		}
		if !a.confirm("Restore? [Y/n] ") {
			fmt.Fprintln(a.Stdout, "Nothing restored.")
			return 1
		}
	}
	if err := a.checkPlan(st, key, plan); err != nil {
		return a.fail(err)
	}
	result, err := a.executeRestore(st, key, plan)
	if err != nil {
		return a.fail(err)
	}
	a.printRestoreResult(plan, result)
	return 0
}

func (a *App) printRestoreResult(plan *restorePlan, result restoreResult) {
	fmt.Fprintf(a.Stdout, "Restored %d sessions (%d files) into %s", result.Sessions, result.Files, a.fmtPath(a.projectsRoot()))
	if result.HistoryLines > 0 {
		fmt.Fprintf(a.Stdout, ", %d prompt-history lines", result.HistoryLines)
	}
	if len(result.SettingsAdded) > 0 {
		fmt.Fprintf(a.Stdout, ", %d settings", len(result.SettingsAdded))
	}
	if result.ConfigWritten > 0 {
		fmt.Fprintf(a.Stdout, ", %d configuration files", result.ConfigWritten)
	}
	if result.Boots > 0 {
		fmt.Fprintf(a.Stdout, ", %d claude-afterlife snapshots", result.Boots)
	}
	fmt.Fprintln(a.Stdout, ".")
	if result.Unchanged > 0 {
		fmt.Fprintf(a.Stdout, "Already up to date: %d files.\n", result.Unchanged)
	}
	if result.ConfigKept > 0 {
		fmt.Fprintf(a.Stdout, "Kept your own copy of %d configuration files.\n", result.ConfigKept)
	}
	printLimited(a.Stdout, "Kept your local copy of files that differ from the backup:", result.Kept)
	skipped := 0
	for _, mapping := range plan.mappings {
		if mapping.New == "" {
			skipped++
		}
	}
	if skipped > 0 {
		fmt.Fprintf(a.Stdout, "Skipped %d projects with no folder; restore them later with -map /old/path=/new/path.\n", skipped)
	}
	if plan.cleanupDays > 0 {
		fmt.Fprintf(a.Stdout, "Set cleanupPeriodDays to %d.\n", plan.cleanupDays)
	}
	fmt.Fprintf(a.Stdout, "Next: %s restore reopens the sessions that were open at the last backup.\n", name)
}
