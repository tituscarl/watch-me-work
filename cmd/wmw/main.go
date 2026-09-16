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
	default:
		fmt.Fprintln(os.Stderr, "usage: wmw [ls | status | wait <project> [state...] | install]  (no args opens the TUI)")
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
	now := time.Now().Unix()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tTITLE\tSTATE\tAGE")
	for _, s := range ss {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Project(), oneLine(s.Title, 50), s.State, age(now-s.Updated))
	}
	return w.Flush()
}

func age(sec int64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	default:
		return fmt.Sprintf("%dh", sec/3600)
	}
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
