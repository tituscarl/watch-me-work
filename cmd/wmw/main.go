// Command wmw, watch me work, tracks Claude Code sessions across repos and
// tells you when one needs you.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

func main() {
	cmd := "tui"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "tui":
		err = runTUI()
	case "hook":
		runHook()
	case "install":
		err = runInstall()
	case "ls":
		err = runLs()
	case "status":
		err = runStatus()
	case "wait":
		err = runWait(os.Args[2:])
	case "config":
		err = runConfig(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version())
	default:
		fmt.Fprintln(os.Stderr, "usage: wmw [ls | status | wait <project> [state...] | config ... | install | version]  (no args opens the TUI)")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wmw:", err)
		os.Exit(1)
	}
}

func runLs() error {
	ss, err := liveSessions()
	if err != nil {
		return err
	}
	if len(ss) == 0 {
		fmt.Println("no live sessions")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tTITLE\tSTATE")
	for _, s := range ss {
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.Project(), oneLine(s.Title, 50), s.State)
	}
	return w.Flush()
}

// runStatus prints one line for a tmux status bar, nothing when idle.
func runStatus() error {
	ss, err := liveSessions()
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, s := range ss {
		counts[s.State]++
	}
	var parts []string
	for _, st := range []string{"blocked", "failed", "working", "idle"} {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
		}
	}
	if len(parts) > 0 {
		fmt.Println(strings.Join(parts, " · "))
	}
	return nil
}

// runWait blocks until a session in the project reaches one of the states,
// then prints the state. Default states are the ones that need a human.
func runWait(args []string) error {
	if len(args) == 0 {
		return errors.New("wait: which project?")
	}
	repo, until := args[0], args[1:]
	if len(until) == 0 {
		until = []string{"idle", "blocked", "failed"}
	}
	for {
		ss, err := liveSessions()
		if err != nil {
			return err
		}
		for _, s := range ss {
			if s.Project() == repo && slices.Contains(until, s.State) {
				fmt.Println(s.State)
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// runConfig prints the settings or sets one:
//
//	wmw config idle both|banner|sound
//	wmw config sound Glass               (any name from `wmw config sounds`, or a file path)
//	wmw config test                      (fire both notifications as configured)
func runConfig(args []string) error {
	c := loadConfig()
	if len(args) == 0 {
		fmt.Printf("idle:  %s   (both, banner, or sound)\nsound: %s\n", c.Idle, c.Sound)
		return nil
	}
	switch {
	case args[0] == "sounds":
		fmt.Println(strings.Join(systemSounds(), "\n"))
		return nil
	case args[0] == "test":
		notify("wmw: idle test", "this is the idle notification", c.Sound, c.Idle)
		time.Sleep(1500 * time.Millisecond) // let the first clip finish
		notify("wmw: alert test", "this is the blocked/failed notification", c.Sound, "both")
		return nil
	case len(args) == 2 && args[0] == "idle" && slices.Contains(notifyStyles, args[1]):
		c.Idle = args[1]
	case len(args) == 2 && args[0] == "sound":
		if _, err := os.Stat(soundPath(args[1])); err != nil {
			return fmt.Errorf("no sound %q; see `wmw config sounds` or give a file path", args[1])
		}
		c.Sound = args[1]
	default:
		return errors.New("usage: wmw config [idle both|banner|sound | sound <name|path> | sounds | test]")
	}
	return saveConfig(c)
}

var hookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Notification", "Stop", "StopFailure", "SessionEnd"}

// runInstall adds our hooks to ~/.claude/settings.json next to whatever is
// already there. Safe to run twice.
func runInstall() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	cmd := "'" + exe + "' hook"

	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".claude", "settings.json")
	settings := map[string]any{}
	mode := fs.FileMode(0o600)
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode()
		}
		os.WriteFile(path+".bak", b, mode)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, ev := range hookEvents {
		list, _ := hooks[ev].([]any)
		// ponytail: substring check over the printed list; fine until it isn't
		if strings.Contains(fmt.Sprint(list), cmd) {
			continue
		}
		entry := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": 5}}}
		if ev == "PreToolUse" {
			entry["matcher"] = "AskUserQuestion|ExitPlanMode"
		}
		hooks[ev] = append(list, entry)
	}
	settings["hooks"] = hooks

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false) // keep other people's ">/dev/null" readable
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return err
	}
	fmt.Println("hooks installed in", path, "(backup at .bak)")
	return nil
}
