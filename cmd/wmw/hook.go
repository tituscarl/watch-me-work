package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	repo := filepath.Base(in.CWD)
	switch in.Event {
	case "Stop":
		notify(repo+" is idle", oneLine(in.LastMsg, 120), false)
	case "StopFailure":
		notify(repo+" failed", e.Text, true)
	case "PreToolUse", "Notification":
		notify(repo+" needs you", e.Text, true)
	}
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

func notify(title, body string, urgent bool) {
	switch runtime.GOOS {
	case "darwin":
		sound := "Glass"
		if urgent {
			sound = "Basso"
		}
		// text goes in via argv so nothing needs escaping inside the script
		exec.Command("osascript",
			"-e", "on run argv",
			"-e", "display notification (item 2 of argv) with title (item 1 of argv) sound name (item 3 of argv)",
			"-e", "end run", title, body, sound).Run()
	default:
		exec.Command("notify-send", title, body).Run()
	}
}
