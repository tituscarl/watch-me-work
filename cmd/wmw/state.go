package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// One line in events.jsonl. The hook writes these, everything else reads them.
type Event struct {
	TS    int64  `json:"ts"`
	Ev    string `json:"ev"`
	SID   string `json:"sid"`
	CWD   string `json:"cwd,omitempty"`
	Pane  string `json:"pane,omitempty"`  // tmux
	ITerm string `json:"iterm,omitempty"` // iTerm2 session uuid
	// Ghostty's AppleScript ids don't match GHOSTTY_SURFACE_ID, so the tty is
	// what ties a session to its terminal there.
	Ghostty string `json:"ghostty,omitempty"`
	Term    string `json:"term,omitempty"` // "vscode" when in its integrated terminal
	Path    string `json:"tp,omitempty"`   // transcript path
	Text    string `json:"text,omitempty"`
}

type Session struct {
	SID, CWD, Pane, ITerm, Ghostty, Term string
	PID                                  int    // agent process from Claude's registry, 0 when unknown
	Path                                 string // transcript, where Claude keeps the title
	Title                                string
	State                                string // working, idle, blocked, failed
	Prompt                               string // last thing the user asked
	Note                                 string // what it is blocked or failed on
	Unread                               bool   // finished or failed and not opened from the TUI since
	Updated                              int64
}

func (s Session) Project() string { return filepath.Base(s.CWD) }

// Sessions killed without a SessionEnd fall off after this long.
const stale = 6 * time.Hour

func statePath(name string) string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "wmw", name)
}

func eventsPath() string { return statePath("events.jsonl") }

// The TUI leaves its pid here while running; notifications only fire while
// something is around to look at. A stale file from a crash fails the liveness
// check, so it needs no cleanup.
func tuiPidPath() string { return statePath("tui.pid") }

func tuiOpen() bool {
	b, err := os.ReadFile(tuiPidPath())
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}

func readEvents() ([]Event, error) {
	f, err := os.Open(eventsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var evs []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		// a torn line from a truncation race is skipped, not fatal
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.SID != "" {
			evs = append(evs, e)
		}
	}
	return evs, sc.Err()
}

var statePrio = map[string]int{"blocked": 0, "failed": 1, "working": 2, "idle": 3}

// fold replays the log into the current state of every live session,
// most urgent first.
func fold(evs []Event, now int64) []Session {
	byID := map[string]*Session{}
	for _, e := range evs {
		if e.Ev == "SessionEnd" {
			delete(byID, e.SID)
			continue
		}
		s := byID[e.SID]
		if s == nil {
			s = &Session{SID: e.SID, State: "idle"}
			byID[e.SID] = s
		}
		if e.CWD != "" {
			s.CWD = e.CWD
		}
		if e.Pane != "" {
			s.Pane = e.Pane
		}
		if e.ITerm != "" {
			s.ITerm = e.ITerm
		}
		if e.Ghostty != "" {
			s.Ghostty = e.Ghostty
		}
		if e.Term != "" {
			s.Term = e.Term
		}
		if e.Path != "" {
			s.Path = e.Path
		}
		if e.Ev != "Seen" { // looking at a session isn't activity in it
			s.Updated = e.TS
		}
		switch e.Ev {
		case "SessionStart":
			s.State, s.Note, s.Unread = "idle", "", false
		case "Stop":
			s.State, s.Note, s.Unread = "idle", "", true
		case "Seen": // written by the TUI when you open the session
			s.Unread = false
		case "UserPromptSubmit":
			s.State, s.Prompt, s.Note, s.Unread = "working", e.Text, "", false
		case "PreToolUse", "Notification":
			s.State, s.Note = "blocked", e.Text
		case "PostToolUse":
			// the only signal that a permission prompt got answered
			if s.State == "blocked" {
				s.State, s.Note = "working", ""
			}
		case "StopFailure":
			s.State, s.Note, s.Unread = "failed", e.Text, true
		}
	}
	var out []Session
	for _, s := range byID {
		if now-s.Updated < int64(stale/time.Second) {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if statePrio[out[i].State] != statePrio[out[j].State] {
			return statePrio[out[i].State] < statePrio[out[j].State]
		}
		if out[i].Updated != out[j].Updated {
			return out[i].Updated > out[j].Updated
		}
		return out[i].SID < out[j].SID // ties would otherwise reshuffle every tick
	})
	return out
}

func liveSessions() ([]Session, error) {
	evs, err := readEvents()
	if err != nil {
		return nil, err
	}
	ss := fold(evs, time.Now().Unix())
	reg := registry()
	out := ss[:0]
	for _, s := range ss {
		r, known := reg[s.SID]
		if known && !r.alive {
			continue // terminal was killed, no SessionEnd ever came
		}
		// Esc fires no hook, so an interrupted turn would sit on "working" forever.
		// Claude's own status flips to idle at that moment, so trust it when it is newer.
		if known && s.State == "working" && r.Status == "idle" && r.StatusAt/1000 >= s.Updated {
			s.State = "idle"
		}
		if known {
			s.PID = r.PID
		}
		s.Title = title(s)
		out = append(out, s)
	}
	return out, nil
}

// Claude Code keeps one file per running session under ~/.claude/sessions.
// ponytail: undocumented, so anything odd about a file just means we ignore it
// and fall back to the hooks.
type registryEntry struct {
	SID      string `json:"sessionId"`
	PID      int    `json:"pid"`
	Status   string `json:"status"`          // busy, idle
	StatusAt int64  `json:"statusUpdatedAt"` // ms
	alive    bool
}

func registry() map[string]registryEntry {
	home, _ := os.UserHomeDir()
	files, _ := filepath.Glob(filepath.Join(home, ".claude", "sessions", "*.json"))
	out := map[string]registryEntry{}
	for _, f := range files {
		var r registryEntry
		if b, err := os.ReadFile(f); err != nil || json.Unmarshal(b, &r) != nil || r.SID == "" {
			continue
		}
		p, err := os.FindProcess(r.PID)
		r.alive = err == nil && p.Signal(syscall.Signal(0)) == nil
		// a resumed session leaves a file per pid; the live one, then the newest, wins
		if prev, ok := out[r.SID]; !ok || (r.alive && !prev.alive) || (r.alive == prev.alive && r.StatusAt > prev.StatusAt) {
			out[r.SID] = r
		}
	}
	return out
}

// Claude rewrites an ai-title record in the transcript every turn, and /rename
// appends a custom-title. Reading the file is cheap but not free, so cache briefly.
var (
	titleMu    sync.Mutex
	titleCache = map[string]struct {
		t  string
		at time.Time
	}{}
)

func title(s Session) string {
	if s.Path == "" {
		return ""
	}
	titleMu.Lock()
	defer titleMu.Unlock()
	if c, ok := titleCache[s.SID]; ok && time.Since(c.at) < 5*time.Second {
		return c.t
	}
	t := lastTitle(s.Path)
	titleCache[s.SID] = struct {
		t  string
		at time.Time
	}{t, time.Now()}
	return t
}

// lastTitle prefers a name the user set with /rename over Claude's own summary.
// ponytail: reads the whole transcript, at most once per 5s per session. Tail-read
// the ai-title and pin the custom-title if this ever shows up in a profile.
func lastTitle(path string) string {
	buf, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, k := range [...]struct{ typ, field string }{{"custom-title", "customTitle"}, {"ai-title", "aiTitle"}} {
		i := bytes.LastIndex(buf, []byte(`"type":"`+k.typ+`"`))
		if i < 0 {
			continue
		}
		start := bytes.LastIndexByte(buf[:i], '\n') + 1
		end := bytes.IndexByte(buf[i:], '\n')
		if end < 0 {
			end = len(buf)
		} else {
			end += i
		}
		var rec map[string]string
		if json.Unmarshal(buf[start:end], &rec) == nil && rec[k.field] != "" {
			return rec[k.field]
		}
	}
	return firstPrompt(buf)
}

// firstPrompt is the fallback for a young session with no title yet, which is
// what Claude's own session picker shows too.
func firstPrompt(buf []byte) string {
	for _, line := range bytes.Split(buf, []byte{'\n'}) {
		if !bytes.Contains(line, []byte(`"type":"user"`)) {
			continue
		}
		var rec struct {
			Sidechain bool `json:"isSidechain"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		var text string
		// content is a string for typed prompts and an array for tool results;
		// slash commands arrive wrapped in <command-...> tags
		if json.Unmarshal(line, &rec) == nil && !rec.Sidechain &&
			json.Unmarshal(rec.Message.Content, &text) == nil && text != "" && text[0] != '<' {
			return oneLine(text, 60)
		}
	}
	return ""
}
