package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Only the fields we use. A PostToolUse payload can carry a whole file in
// tool_response; none of that should land in events.jsonl.
type hookInput struct {
	Event     string `json:"hook_event_name"`
	SID       string `json:"session_id"`
	CWD       string `json:"cwd"`
	Path      string `json:"transcript_path"`
	AgentID   string `json:"agent_id"`
	Prompt    string `json:"prompt"`
	Tool      string `json:"tool_name"`
	NotifType string `json:"notification_type"`
	Message   string `json:"message"`
	LastMsg   string `json:"last_assistant_message"`
	Error     string `json:"error"`
}

const maxLog = 5 << 20

// runHook is what Claude Code calls. It never fails and never prints:
// output on PreToolUse would be read as a permission decision.
func runHook() {
	// Cursor's embedded Claude and background jobs aren't sessions of ours.
	if os.Getenv("CLAUDE_JOB_DIR") != "" || os.Getenv("CURSOR_VERSION") != "" {
		return
	}
	var in hookInput
	json.NewDecoder(os.Stdin).Decode(&in)
	if in.SID == "" || in.AgentID != "" { // subagents ride on the parent's row
		return
	}
	e := Event{TS: time.Now().Unix(), Ev: in.Event, SID: in.SID, CWD: in.CWD, Path: in.Path}
	// TERM_PROGRAM says which terminal this really is. TMUX_PANE and
	// ITERM_SESSION_ID get inherited by anything launched from that shell,
	// VS Code included, so they can't be trusted on their own.
	// ITERM_SESSION_ID looks like w0t2p1:UUID; AppleScript wants the UUID.
	_, iterm, _ := strings.Cut(os.Getenv("ITERM_SESSION_ID"), ":")
	switch os.Getenv("TERM_PROGRAM") {
	case "tmux":
		e.Pane, e.ITerm = os.Getenv("TMUX_PANE"), iterm // tmux inside iTerm2 wants both
	case "iTerm.app":
		e.ITerm = iterm
	case "ghostty":
		e.Ghostty = ttyOf(os.Getppid())
	case "vscode":
		e.Term = "vscode"
	}
	switch in.Event {
	case "UserPromptSubmit":
		e.Text = in.Prompt
	case "PreToolUse":
		e.Text = in.Tool
	case "Notification":
		if in.NotifType != "permission_prompt" && in.NotifType != "elicitation_dialog" {
			return // idle_prompt and auth_success change nothing
		}
		e.Text = in.Message
	case "StopFailure":
		e.Text = in.Error
	}
	e.Text = oneLine(e.Text, 200)
	appendEvent(e)
	// finishing while you're already looking at it doesn't count as unread
	if (in.Event == "Stop" || in.Event == "StopFailure") && focused(e) {
		appendEvent(Event{TS: e.TS, Ev: "Seen", SID: e.SID})
	}
	if !tuiOpen() {
		return
	}

	repo := filepath.Base(in.CWD)
	c := loadConfig()
	switch in.Event {
	case "Stop":
		notify(repo+" is idle", oneLine(in.LastMsg, 120), c.Sound, c.Idle)
	case "StopFailure":
		notify(repo+" failed", e.Text, c.Sound, "both")
	case "PreToolUse", "Notification":
		notify(repo+" needs you", e.Text, c.Sound, "both")
	}
}

// User settings, written by `wmw config`. Only read when about to notify.
type config struct {
	Idle  string `json:"idle"`  // both, banner, sound
	Sound string `json:"sound"` // system sound name or a path to an audio file
}

var notifyStyles = []string{"both", "banner", "sound"}

const soundDir = "/System/Library/Sounds"

// soundPath accepts a bare system sound name like Glass or a path to any file afplay can play.
func soundPath(s string) string {
	if strings.Contains(s, "/") {
		return s
	}
	return filepath.Join(soundDir, s+".aiff")
}

func systemSounds() []string {
	paths, _ := filepath.Glob(filepath.Join(soundDir, "*.aiff"))
	var names []string
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(p), ".aiff"))
	}
	return names
}

func playSound(s string) {
	// Start, not Run: afplay blocks for the length of the clip and Claude
	// waits for the hook to exit. Its output goes to /dev/null by default,
	// so it doesn't hold the hook's stdout open either.
	exec.Command("afplay", soundPath(s)).Start()
}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "wmw", "config.json")
}

func loadConfig() config {
	c := config{Idle: "both"}
	if b, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(b, &c)
		// briefly there were two sounds; keep the idle one if that's what is on disk
		var old struct {
			IdleSound string `json:"idleSound"`
		}
		if json.Unmarshal(b, &old); c.Sound == "" {
			c.Sound = old.IdleSound
		}
	}
	if c.Sound == "" { // default applied last so the legacy check above sees an empty value
		c.Sound = "Glass"
	}
	return c
}

func saveConfig(c config) error {
	os.MkdirAll(filepath.Dir(configPath()), 0o700)
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), append(b, '\n'), 0o600)
}

// focused reports whether the session's terminal is the one in front right now.
// VS Code can't be asked which terminal tab is showing, so it's never focused here.
func focused(e Event) bool {
	if e.Pane != "" {
		out, err := exec.Command("tmux", "display", "-p", "-t", e.Pane, "#{pane_active} #{window_active} #{session_attached}").Output()
		f := strings.Fields(string(out))
		if err != nil || len(f) != 3 || f[0] != "1" || f[1] != "1" || f[2] == "0" {
			return false
		}
		if e.ITerm == "" {
			return true
		}
		// tmux inside iTerm2: the pane is active, now the tab must be in front too
	}
	if e.Ghostty != "" {
		out, err := exec.Command("osascript",
			"-e", `tell application "Ghostty"`,
			"-e", `if not frontmost then return ""`,
			"-e", `return tty of focused terminal of selected tab of front window`,
			"-e", `end tell`).Output()
		return err == nil && strings.TrimSpace(string(out)) == e.Ghostty
	}
	if e.ITerm == "" {
		return false
	}
	out, err := exec.Command("osascript",
		"-e", `tell application "iTerm2"`,
		"-e", `if not frontmost then return ""`,
		"-e", `return id of current session of current window`,
		"-e", `end tell`).Output()
	return err == nil && strings.TrimSpace(string(out)) == e.ITerm
}

// ttyOf returns the controlling terminal of pid or its nearest ancestor that
// has one. Hooks run detached, but the agent that spawned them still owns the tty.
// ponytail: ttys get reused. The jump looks the tty up again from the live
// pid, so a stale one only matters when Claude's registry has no pid for it.
func ttyOf(pid int) string {
	for range 5 {
		out, err := exec.Command("ps", "-o", "tty=,ppid=", "-p", strconv.Itoa(pid)).Output()
		f := strings.Fields(string(out))
		if err != nil || len(f) != 2 {
			return ""
		}
		if strings.Trim(f[0], "?") != "" {
			return "/dev/" + f[0]
		}
		if pid, _ = strconv.Atoi(f[1]); pid <= 1 {
			return ""
		}
	}
	return ""
}

func appendEvent(e Event) {
	p := eventsPath()
	os.MkdirAll(filepath.Dir(p), 0o700)
	// ponytail: past 5MB the log is wiped and idle sessions reappear on their
	// next event. Compact down to live sessions instead if that ever hurts.
	if st, err := os.Stat(p); err == nil && st.Size() > maxLog {
		os.Truncate(p, 0)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	f.Write(append(b, '\n')) // one write under 4KB, so concurrent hooks can't interleave
}

// oneLine collapses whitespace and caps the result at n runes, ellipsis included,
// so it always fits a %-ns column.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// notify shows a banner, plays a sound, or both. The sound goes through afplay
// rather than the banner's "sound name": that one only plays if Notification
// Center's per-app sound toggle happens to be on. Linux gets a banner only.
func notify(title, body, sound, style string) {
	if runtime.GOOS != "darwin" {
		exec.Command("notify-send", title, body).Run()
		return
	}
	if style != "banner" {
		playSound(sound)
	}
	if style != "sound" {
		// text goes in via argv so nothing needs escaping inside the script
		exec.Command("osascript", "-e", "on run argv",
			"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
			"-e", "end run", title, body).Run()
	}
}
