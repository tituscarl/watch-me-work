package main

import (
	"os"
	"testing"
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
