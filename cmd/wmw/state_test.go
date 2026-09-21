package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestFold(t *testing.T) {
	ev := func(ts int64, name, sid, text string) Event {
		return Event{TS: ts, Ev: name, SID: sid, CWD: "/repos/" + sid, Text: text}
	}
	const now = 100_000
	evs := []Event{
		// a: full round trip through a permission prompt, ends idle
		ev(now-50, "SessionStart", "a", ""),
		ev(now-40, "UserPromptSubmit", "a", "fix  the\ntests"),
		ev(now-30, "Notification", "a", "permission for Bash"),
		ev(now-20, "PostToolUse", "a", ""),
		ev(now-10, "Stop", "a", ""),
		// b: stuck on a question, never saw SessionStart
		ev(now-9, "UserPromptSubmit", "b", "refactor"),
		ev(now-8, "PreToolUse", "b", "AskUserQuestion"),
		// c: closed
		ev(now-7, "UserPromptSubmit", "c", "x"),
		ev(now-6, "SessionEnd", "c", ""),
		// d: killed terminal, long ago
		ev(1, "UserPromptSubmit", "d", "x"),
	}

	got := fold(evs, now)
	want := []struct{ sid, state, prompt, note string }{
		{"b", "blocked", "refactor", "AskUserQuestion"},
		{"a", "idle", "fix  the\ntests", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sessions, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.SID != w.sid || g.State != w.state || g.Prompt != w.prompt || g.Note != w.note {
			t.Errorf("row %d: got %+v, want %+v", i, g, w)
		}
	}

	// mid-flight: blocked flips back to working on the next tool result
	mid := fold(evs[:4], now)
	if mid[0].State != "working" || mid[0].Note != "" {
		t.Errorf("after PostToolUse: %+v", mid[0])
	}
	// unread: set by Stop, cleared by opening it or typing into it
	if !got[1].Unread {
		t.Error("a should be unread after Stop")
	}
	seen := fold(append(evs, Event{TS: now, Ev: "Seen", SID: "a"}), now)
	if seen[1].Unread || seen[1].Updated != now-10 {
		t.Errorf("Seen should clear unread without touching Updated: %+v", seen[1])
	}
	for _, s := range fold(append(evs, Event{TS: now, Ev: "UserPromptSubmit", SID: "a", Text: "more"}), now) {
		if s.SID == "a" && s.Unread {
			t.Errorf("typing should clear unread: %+v", s)
		}
	}
	if oneLine("fix  the\ntests", 60) != "fix the tests" {
		t.Errorf("oneLine: %q", oneLine("fix  the\ntests", 60))
	}
}

func TestRows(t *testing.T) {
	m := model{open: map[string]bool{}, sessions: []Session{
		{SID: "l3", CWD: "/r/luna", State: "blocked"},
		{SID: "a1", CWD: "/r/aw", State: "working"},
		{SID: "l1", CWD: "/r/luna", State: "working"},
		{SID: "l2", CWD: "/r/luna", State: "idle"},
	}}
	rs := m.rows()
	if len(rs) != 2 || rs[0].n != 3 || rs[0].s.SID != "l3" || rs[1].key != "a1" {
		t.Fatalf("collapsed: %+v", rs)
	}
	m.open["luna"] = true
	rs = m.rows()
	if len(rs) != 5 || !rs[1].child || rs[3].s.SID != "l2" || rs[4].key != "a1" {
		t.Fatalf("expanded: %+v", rs)
	}
}

func TestLastTitle(t *testing.T) {
	p := t.TempDir() + "/t.jsonl"
	os.WriteFile(p, []byte(`{"type":"ai-title","aiTitle":"old","sessionId":"x"}
{"type":"assistant","message":"lots of stuff"}
{"type":"ai-title","aiTitle":"Terminal-based Orca Ade alternative","sessionId":"x"}
{"type":"last-prompt","lastPrompt":"hi"}
`), 0o600)
	if got := lastTitle(p); got != "Terminal-based Orca Ade alternative" {
		t.Errorf("lastTitle = %q", got)
	}
	if lastTitle(p+".missing") != "" {
		t.Error("missing file should give empty title")
	}
	// a /rename earlier in the file beats a newer ai-title
	os.WriteFile(p, []byte(`{"type":"custom-title","customTitle":"cascade-parent-bg-settings","sessionId":"x"}
{"type":"ai-title","aiTitle":"Some generated summary","sessionId":"x"}
`), 0o600)
	if got := lastTitle(p); got != "cascade-parent-bg-settings" {
		t.Errorf("custom title should win, got %q", got)
	}
	// no title records yet: first real prompt, skipping slash commands and tool results
	os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}
{"type":"user","isSidechain":true,"message":{"role":"user","content":"subagent prompt"}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}
{"type":"user","message":{"role":"user","content":"okay"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Sure."}]}}
`), 0o600)
	if got := lastTitle(p); got != "okay" {
		t.Errorf("first prompt fallback, got %q", got)
	}
}

func TestTuiOpen(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if tuiOpen() {
		t.Error("no pid file should read as closed")
	}
	os.MkdirAll(filepath.Dir(tuiPidPath()), 0o700)
	os.WriteFile(tuiPidPath(), []byte(strconv.Itoa(os.Getpid())), 0o600)
	if !tuiOpen() {
		t.Error("own pid should read as open")
	}
	os.WriteFile(tuiPidPath(), []byte("999999"), 0o600) // nothing runs there
	if tuiOpen() {
		t.Error("dead pid should read as closed")
	}
}

func TestConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if loadConfig().Idle != "both" {
		t.Error("default should be both")
	}
	if err := runConfig([]string{"idle", "loud"}); err == nil {
		t.Error("bad style should be rejected")
	}
	if err := runConfig([]string{"idle", "sound"}); err != nil {
		t.Fatal(err)
	}
	if loadConfig().Idle != "sound" {
		t.Error("setting did not persist")
	}
	if err := runConfig([]string{"sound", "NoSuchSound"}); err == nil {
		t.Error("unknown sound should be rejected")
	}
	if err := runConfig([]string{"sound", "Hero"}); err != nil {
		t.Skip("no system sounds here:", err) // linux CI
	}
	if loadConfig().Sound != "Hero" {
		t.Error("sound did not persist")
	}
	custom := t.TempDir() + "/ding.aiff"
	os.WriteFile(custom, []byte("x"), 0o600)
	if err := runConfig([]string{"sound", custom}); err != nil || loadConfig().Sound != custom {
		t.Errorf("custom path: err=%v cfg=%+v", err, loadConfig())
	}
	// a config written by the short-lived two-sound version keeps its idle sound
	os.WriteFile(configPath(), []byte(`{"idle":"both","idleSound":"Pop","alertSound":"Sosumi"}`), 0o600)
	if loadConfig().Sound != "Pop" {
		t.Errorf("legacy idleSound not picked up: %+v", loadConfig())
	}
}

func TestRegistryCorrections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", home)
	os.MkdirAll(home+"/.claude/sessions", 0o700)
	os.MkdirAll(home+"/wmw", 0o700)
	now := time.Now().Unix()
	write := func(name string, e Event) {
		b, _ := json.Marshal(e)
		f, _ := os.OpenFile(eventsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		f.Write(append(b, '\n'))
		f.Close()
	}
	write("", Event{TS: now - 60, Ev: "UserPromptSubmit", SID: "esc", CWD: "/r/esc", Text: "x"})
	write("", Event{TS: now - 60, Ev: "UserPromptSubmit", SID: "busy", CWD: "/r/busy", Text: "y"})
	write("", Event{TS: now - 60, Ev: "UserPromptSubmit", SID: "killed", CWD: "/r/killed", Text: "z"})
	write("", Event{TS: now - 60, Ev: "UserPromptSubmit", SID: "unknown", CWD: "/r/unknown", Text: "w"})
	me := os.Getpid()
	reg := func(file, sid string, pid int, status string, at int64) {
		b, _ := json.Marshal(map[string]any{"sessionId": sid, "pid": pid, "status": status, "statusUpdatedAt": at * 1000})
		os.WriteFile(home+"/.claude/sessions/"+file, b, 0o600)
	}
	reg("1.json", "esc", me, "idle", now-30)     // interrupted after the prompt: should read idle
	reg("2.json", "busy", me, "busy", now-30)    // still going
	reg("3.json", "killed", 999999, "idle", now) // dead pid: should vanish
	// "unknown" has no registry file: left alone

	ss, err := liveSessions()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range ss {
		got[s.SID] = s.State
	}
	want := map[string]string{"esc": "idle", "busy": "working", "unknown": "working"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for sid, st := range want {
		if got[sid] != st {
			t.Errorf("%s: got %s, want %s", sid, got[sid], st)
		}
	}
}
