# `claude-afterlife`

Reopen [Claude Code](https://docs.claude.com/en/docs/claude-code) sessions you lost to a reboot or a terminal quitting, each in its own tab, in the directory it was started from.

Claude Code saves every conversation to disk, so `claude --resume <session-id>` can bring one back, as long as you remember which sessions were open and where. That is the part that gets lost: a reboot ends every process, and most terminals (Ghostty, iTerm2, Terminal, kitty) end their sessions as soon as you quit them. `claude-afterlife` keeps track for you.

## How it works

- `claude-afterlife snapshot` runs every minute in the background. It reads the session files Claude Code keeps in `~/.claude/sessions/`, keeps the ones whose process is alive, and records them in a per-boot file under `~/.local/state/claude-afterlife/boots/`. A run takes a few millisec
- Each boot gets its own file, keyed by the OS boot id, so the first snapshot after a reboot doesn't overwrite what was running before it
- `claude-afterlife restore` reopens what you lost since the last restore: the sessions that were running when the machine went down, and the latest batch of sessions that ended during this boot, which is what a terminal quitting or crashing looks like. Each one opens in a new tab running `claude --resume <id>`, and you are left in your shell when Claude exits

Sessions you closed one by one earlier in the day are not reopened: only sessions that ended within `-recent` (10 minutes by default) of the latest loss count as part of it.

## Install

With Homebrew:

```sh
brew install balintb/tap/claude-afterlife
```

Or with Go:

```sh
go install github.com/balintb/claude-afterlife@latest
```

Then, on macOS, install with:

```sh
# macOS only
claude-afterlife install
```

This writes a launchd agent (`~/Library/LaunchAgents/local.claude-afterlife.plist`) that runs the snapshot every minute and at login, and starts it. A Homebrew copy is run through Homebrew's `opt` link, which follows upgrades, so `brew upgrade` does not break the agent. `-interval 30s` changes the frequency and `-no-load` writes the agent without starting it. `claude-afterlife uninstall` removes the agent. If you remove the binary without it, for example with `brew uninstall`, the agent notices within a minute and removes itself.

### Linux

Download a binary from the [releases page](https://github.com/balintb/claude-afterlife/releases) or use `go install`, then run the snapshot every minute from cron:

```sh
( crontab -l 2>/dev/null; echo '* * * * * $HOME/go/bin/claude-afterlife snapshot' ) | crontab -
```

## Usage

After a reboot, or after your terminal quit, run this in the terminal you want the sessions back in:

```sh
claude-afterlife restore
```

It lists what it found, asks for confirmation, then opens one tab per session.

```text
$ claude-afterlife restore
Running before the reboot (boot 73E4DF38, last snapshot 2026-10-07 18:42):
  reopen              5836d989  started 2026-10-06 13:21  api-refactor  ~/code/api
  reopen              4f074334  started 2026-10-07 08:50  docs-pass  ~/code/api
  missing directory   215234f6  started 2026-10-05 13:47  spike  ~/code/api-spike
Reopen 2 session(s) in ghostty? [Y/n]
```

### Terminals

`restore` opens the sessions in the terminal you run it from. Pass `-terminal <name>` to choose another.

| Terminal | `-terminal` | How sessions open | Notes |
|---|---|---|---|
| [Ghostty](https://ghostty.org) 1.3+ | `ghostty` | Tabs in the current window, or a new window | macOS only, through Ghostty's AppleScript support |
| [iTerm2](https://iterm2.com) | `iterm` | Tabs in the current window, or a new window | macOS only, through AppleScript |
| Terminal | `terminal` | One window per session | macOS only. Terminal.app cannot open tabs from a script |
| [kitty](https://sw.kovidgoyal.net/kitty/) | `kitty` | Tabs in the current window, or a new window | Needs `allow_remote_control yes` in `kitty.conf`. From outside kitty it also needs `listen_on` |
| [ThinkTerm](https://github.com/RoversX/thinkterm) | `thinkterm` | Tabs in the current window, or a new window | |
| [WezTerm](https://wezterm.org) | `wezterm` | Tabs in the current window, or a new window | |
| tmux | `tmux` | One tmux window per session | Used whenever `restore` runs inside tmux |
| anything else | `print` | Prints `cd <dir> && claude --resume <id>` lines | Run them yourself |

The first time `restore` drives Ghostty, iTerm2 or Terminal, macOS asks whether your terminal may control that app. Allow it in the dialog or later in System Settings > Privacy & Security > Automation.

### Other commands

```sh
claude-afterlife list                      # boots on record, and the sessions seen during the previous one
claude-afterlife restore -dry-run          # show what would be reopened
claude-afterlife restore -terminal print   # print the commands instead of opening tabs
claude-afterlife restore -all              # every session that ended, not only the latest loss
claude-afterlife restore -boot previous    # only what the last reboot took down
claude-afterlife snapshot                  # take a snapshot now
claude-afterlife uninstall                 # remove the launchd agent (-purge also deletes snapshots)
```

### Restore flags

Flags take one dash or two, so `-dry-run` and `--dry-run` both work.

| Flag | Meaning |
|---|---|
| `-terminal auto\|ghostty\|iterm\|terminal\|kitty\|thinkterm\|wezterm\|tmux\|print` | Where to reopen sessions. `auto` is the terminal `restore` runs in |
| `-boot auto\|previous\|current\|<id prefix>` | `auto` offers everything lost since the last restore. `previous` is what the last reboot took down, `current` the latest sessions that ended during this boot |
| `-new-window` | Open the tabs in a new window even when run inside the terminal |
| `-recent 10m` | How close to the latest loss a session must have ended to be reopened |
| `-all` | Every session that ended, however long ago |
| `-include-background` | Also reopen non-interactive sessions, such as background jobs |
| `-claude-command "claude --model opus"` | Command that starts Claude Code, flags allowed. Default `claude` |
| `-dry-run` | Show the plan and open nothing |
| `-y`, `-yes` | Do not ask for confirmation. Required when `restore` does not run in a terminal, for example from a script |

Sessions that are already running, whose directory no longer exists (a deleted git worktree, for example), or whose conversation file is gone are listed and skipped. Once a restore has opened something, the sessions lost to the reboot are not offered again in that boot; `-boot previous` still shows them.

### Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `CLAUDE_CONFIG_DIR` | `~/.claude` | Claude Code's configuration directory, if you moved it |
| `CLAUDE_AFTERLIFE_DIR` | `$XDG_STATE_HOME/claude-afterlife` or `~/.local/state/claude-afterlife` | Where snapshots are stored |
| `CLAUDE_AFTERLIFE_TERMINAL` | `auto` | Default for `restore -terminal` |
| `CLAUDE_AFTERLIFE_CLAUDE` | `claude` | Default for `restore -claude-command` |

The background job does not see your shell's environment. `claude-afterlife install` copies `CLAUDE_CONFIG_DIR` and the snapshot directory into its launchd agent, so set them before installing. With cron, set any of these you changed for the job as well, or the job and `restore` will look in different places.

## Looking for one particular session?

Try [cz](https://github.com/balintb/cz). Run `cz` in a project, pick any past Claude Code session from a fuzzy list, and it resumes it, no session IDs to remember. `claude-afterlife` brings back the set of sessions you had open; `cz` finds that one from last Tuesday.

```sh
brew install balintb/tap/cz
cz init   # one-time: adds the SessionEnd hook cz uses to log your sessions
```

`cz` might gain what `claude-afterlife` does in a future release.

## Caveats

- `claude-afterlife` reads `~/.claude/sessions/*.json`, an internal, undocumented file format of Claude Code. It was built against Claude Code 2.1.2xx and may stop working if that format changes. It only reads the `.json` files there, doesn't write to `~/.claude`
- Resuming brings back the conversation, not work that was in flight. A tool call, background shell command or subagent that was running when the session ended is gone, so ask Claude to pick up where it left off
- Each session reopens in a new tab with the same working directory but a fresh environment. Anything you exported by hand in the original shell is not carried over
- Terminals with their own session restore (ThinkTerm for example) may also bring back plain shells for the same tabs after a reboot. You may want to close those duplicates
- A session you close a few minutes before a reboot or a terminal quit counts as part of that loss and is reopened. Close it again, or pass a smaller `-recent`

## Privacy

Snapshots hold session ids, working directories, session names and process ids, and are written with owner-only permissions. **Nothing leaves your machine, and claude-afterlife makes no network requests.**

## Development

Standard library only, no dependencies.

```sh
go test -race ./...
go vet ./...
git config core.hooksPath .githooks   # reject commit subjects release-please cannot read
```

Tests don't open terminal windows or load launchd jobs - every terminal is driven through a fake command runner, and the AppleScripts are only compiled with `osacompile`, which does not launch apps. One test loads a throwaway launchd agent to check that it removes itself once its binary is gone; it runs only with `CLAUDE_AFTERLIFE_LAUNCHD_TEST=1`.

## License

MIT. `claude-afterlife` is not affiliated with or endorsed by Anthropic.
