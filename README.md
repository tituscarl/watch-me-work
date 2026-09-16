# watch-me-work (`wmw`)

A terminal tool for watching my AI coding agents work in real time. I often lose track of which session is waiting on a permission or an approval, and which ones have finished versus still running. Orca ADE covers this, but I wanted a lightweight CLI/TUI version since I'm in a terminal most of the time. I plan to support more terminals and coding agents in the future, time permitting.

Feel free to try it!

## Install

Requires Go 1.26 or newer and macOS.

```sh
go install github.com/tituscarl/watch-me-work/cmd/wmw@latest
wmw install
```

`wmw install` adds hook entries to `~/.claude/settings.json` so Claude Code reports session events. Existing hooks are left untouched and a backup is written next to the file. It is safe to run again after updating.

Then run `wmw` to open the dashboard. `wmw ls`, `wmw status`, and `wmw wait <project>` give the same information in script-friendly form.

## Supported coding agents

- Claude Code (for now)

## Supported terminals

Sessions are tracked in any terminal. Jumping to a session from the TUI works in:

- iTerm2
- VS Code integrated terminal
