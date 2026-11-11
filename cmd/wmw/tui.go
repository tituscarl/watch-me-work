package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// projW is the PROJECT column width; the title takes what the fixed columns leave.
const projW = 32

var (
	stateStyle = map[string]lipgloss.Style{
		"blocked": lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"}).Bold(true),
		"failed":  lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "127", Dark: "170"}).Bold(true),
		"working": lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "136", Dark: "220"}), // yellow, kept clear of Claude's orange ✳
		"idle":    lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "78"}),
	}
	dim    = lipgloss.NewStyle().Faint(true)
	bold   = lipgloss.NewStyle().Bold(true)
	plain  = lipgloss.NewStyle()
	unread = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "26", Dark: "75"}).Bold(true)
	barBg  = lipgloss.AdaptiveColor{Light: "254", Dark: "237"} // selected row
	// braille spinner for working, one frame per 100ms redraw
	spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
)

// stateGlyph gives each state its own shape as well as its color, so blocked
// and idle stay apart for anyone who can't tell red from green.
func stateGlyph(state string) (string, lipgloss.Style) {
	g := "●"
	switch state {
	case "blocked":
		g = "▲"
	case "failed":
		g = "✖"
	case "working":
		g = spinner[time.Now().UnixMilli()/100%int64(len(spinner))]
	}
	return g, stateStyle[state]
}

type (
	tickMsg     time.Time
	sessionsMsg []Session
	jumpMsg     string
)

type model struct {
	sessions  []Session
	open      map[string]bool // expanded project groups
	sel       string          // key of the row under the cursor; rows move as states change
	cursor    int
	hx, hy    int // mouse position, hy is -1 when unknown
	width     int
	msg       string
	cfg       config
	lastLoad  time.Time // reload is slower than the redraw, see Update
	lastFocus time.Time
}

// version is the module version go install stamped in, "(devel)" for a plain go build
// outside a tagged checkout.
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.Main.Version
	}
	return "unknown"
}

// settingsRow is the screen line of the idle-notification setting, kept in one
// place so View draws it where Update expects clicks.
func (m model) settingsRow() int {
	if n := len(m.rows()); n > 0 {
		return n + 3 // header, rows, blank, cwd
	}
	return 2 // "no live sessions", blank
}

func (m *model) cycleIdle() {
	i := slices.Index(notifyStyles, m.cfg.Idle)
	m.cfg.Idle = notifyStyles[(i+1)%len(notifyStyles)]
	m.save()
}

// cycleSound steps through the system sounds and previews the new one.
// A custom file path in the config restarts from the first system sound.
func (m *model) cycleSound() {
	names := systemSounds()
	if len(names) == 0 {
		return
	}
	m.cfg.Sound = names[(slices.Index(names, m.cfg.Sound)+1)%len(names)]
	playSound(m.cfg.Sound)
	m.save()
}

func (m *model) save() {
	if err := saveConfig(m.cfg); err != nil {
		m.msg = "could not save config: " + err.Error()
	}
}

// row is one visible line: a lone session, a project header, or a child under one.
type row struct {
	key     string // SID, or "grp:"+project for a header
	project string
	s       Session // the session, or the most urgent one for a header
	n       int     // sessions in the group, >1 marks a header
	child   bool
	unread  bool // the session, or any session in the group
}

// rows groups sessions by project, keeping the urgency order fold gave us.
func (m model) rows() []row {
	var order []string
	groups := map[string][]Session{}
	for _, s := range m.sessions {
		p := s.Project()
		if _, seen := groups[p]; !seen {
			order = append(order, p)
		}
		groups[p] = append(groups[p], s)
	}
	var out []row
	for _, p := range order {
		ss := groups[p]
		if len(ss) == 1 {
			out = append(out, row{key: ss[0].SID, project: p, s: ss[0], unread: ss[0].Unread})
			continue
		}
		hdr := row{key: "grp:" + p, project: p, s: ss[0], n: len(ss)}
		for _, s := range ss {
			hdr.unread = hdr.unread || s.Unread
		}
		out = append(out, hdr)
		if m.open[p] {
			for _, s := range ss {
				out = append(out, row{key: s.SID, project: p, s: s, child: true, unread: s.Unread})
			}
		}
	}
	return out
}

func runTUI() error {
	// ponytail: last TUI to start owns the pid file; closing it silences
	// notifications even if another one is still open
	os.MkdirAll(filepath.Dir(tuiPidPath()), 0o700)
	os.WriteFile(tuiPidPath(), []byte(strconv.Itoa(os.Getpid())), 0o600)
	defer os.Remove(tuiPidPath())
	_, err := tea.NewProgram(model{width: 80, hy: -1, open: map[string]bool{}, cfg: loadConfig()}, tea.WithAltScreen(), tea.WithMouseAllMotion()).Run()
	return err
}

func reload() tea.Msg {
	ss, _ := liveSessions()
	return sessionsMsg(ss)
}

// seenIfFocused clears the dot on a session you switched to yourself. The hook
// only catches the case where you were already on it when it stopped.
// ponytail: one osascript per unread session every 2s. If that gets heavy, ask
// the terminal for its front session once and compare against all of them.
func seenIfFocused(ss []Session) tea.Cmd {
	return func() tea.Msg {
		for _, s := range ss {
			if s.Unread && focused(Event{Pane: s.Pane, ITerm: s.ITerm, Ghostty: s.Ghostty}) {
				appendEvent(Event{TS: time.Now().Unix(), Ev: "Seen", SID: s.SID})
			}
		}
		return nil
	}
}

func tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Init() tea.Cmd { return tea.Batch(reload, tick()) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tickMsg:
		// the animation wants a 100ms redraw, the session list does not; reading
		// the log and stat-ing every pid that often is pure waste
		if t := time.Time(msg); t.Sub(m.lastLoad) >= 500*time.Millisecond {
			m.lastLoad = t
			if t.Sub(m.lastFocus) >= 2*time.Second {
				m.lastFocus = t
				return m, tea.Batch(reload, tick(), seenIfFocused(m.sessions))
			}
			return m, tea.Batch(reload, tick())
		}
		return m, tick()
	case sessionsMsg:
		m.sessions = msg
		m.cursor = 0
		for i, r := range m.rows() {
			if r.key == m.sel {
				m.cursor = i
			}
		}
	case jumpMsg:
		m.msg = string(msg)
	case tea.MouseMsg:
		m.hx, m.hy = msg.X, msg.Y
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.Y == m.settingsRow() {
			m.cycleIdle()
			return m, nil
		}
		// header is row 0, sessions start at row 1
		if r := msg.Y - 1; msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && r >= 0 && r < len(m.rows()) {
			m.cursor = r
			return m.activate()
		}
	case tea.KeyMsg:
		m.hy = -1 // keyboard use drops any stale hover hint
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "j", "down":
			if m.cursor < len(m.rows())-1 {
				m.cursor++
			}
		case "k", "up":
			if m.cursor > 0 {
				m.cursor--
			}
		case "enter", " ":
			return m.activate()
		case "n":
			m.cycleIdle()
		case "s":
			m.cycleSound()
		}
	}
	if rs := m.rows(); m.cursor < len(rs) {
		m.sel = rs[m.cursor].key
	}
	return m, nil
}

// activate toggles a project group or focuses a session's terminal.
// The jump runs off the update loop; the AppleScript takes a few hundred ms.
func (m model) activate() (tea.Model, tea.Cmd) {
	rs := m.rows()
	if m.cursor >= len(rs) {
		return m, nil
	}
	r := rs[m.cursor]
	m.sel = r.key
	if r.n > 1 {
		m.open[r.project] = !m.open[r.project]
		return m, nil
	}
	// opening counts as having seen it, whether or not the jump lands
	appendEvent(Event{TS: time.Now().Unix(), Ev: "Seen", SID: r.s.SID})
	return m, tea.Batch(reload, func() tea.Msg { return jumpMsg(jump(r.s)) })
}

func (m model) View() string {
	var b strings.Builder
	rs := m.rows()
	if len(rs) == 0 {
		b.WriteString(dim.Render("no live sessions") + "\n")
	} else {
		tw := max(20, m.width-projW-7) // mark, gaps, state dot, unread dot
		b.WriteString(dim.Render(fmt.Sprintf("  %-*s     %-*s", projW, "PROJECT", tw, "TITLE")) + "\n")
		for i, r := range rs {
			sel := i == m.cursor
			// every segment gets the bar background on the selected row; styling the
			// whole line at once would reset the state color mid-row
			paint := func(st lipgloss.Style, text string) string {
				if sel {
					st = st.Background(barBg)
				}
				return st.Render(text)
			}
			name := r.project
			switch {
			case r.child:
				name = "  ├─"
				if i+1 >= len(rs) || !rs[i+1].child {
					name = "  └─"
				}
			case r.n > 1 && m.open[r.project]:
				name = fmt.Sprintf("▾ %s (%d)", r.project, r.n)
			case r.n > 1:
				name = fmt.Sprintf("▸ %s (%d)", r.project, r.n)
			}
			name = fmt.Sprintf("%-*s", projW, oneLine(name, projW))
			mark, nameStyle := "  ", plain
			if sel {
				mark, nameStyle = "▌ ", bold
			}
			line := paint(plain, mark) + paint(nameStyle, name) + paint(plain, " ")
			// unread dot sits after the state dot; a header carries it for any child
			flag := " "
			if r.unread {
				flag = "•"
			}
			dot, dotStyle := stateGlyph(r.s.State)
			if r.n > 1 {
				// a header shows its most urgent child's state, so a collapsed group
				// can't hide a blocked session; hovering the count explains the rest
				line += paint(dotStyle, dot) + paint(unread, " "+flag)
				if m.hy == i+1 && overCount(r, m.hx) {
					line += paint(dim, " "+oneLine(m.countHint(r), tw))
				}
			} else {
				line += paint(dotStyle, dot) + paint(unread, " "+flag) +
					paint(plain, fmt.Sprintf(" %-*s", tw, oneLine(r.s.Title, tw)))
			}
			if sel {
				line += paint(plain, strings.Repeat(" ", max(0, m.width-lipgloss.Width(line))))
			}
			b.WriteString(line + "\n")
		}
		if m.cursor < len(rs) {
			b.WriteString("\n" + dim.Render(rs[m.cursor].s.CWD) + "\n")
		}
	}
	// keep in step with settingsRow: this must land on that line
	if len(rs) == 0 {
		b.WriteString("\n")
	}
	b.WriteString(dim.Render("idle: ") + m.cfg.Idle + dim.Render(" · sound: ") + m.cfg.Sound + dim.Render("   n / s to change   "+version()) + "\n")
	// below the settings line so it doesn't shift any clickable row
	var legend []string
	for _, st := range []string{"working", "blocked", "failed", "idle"} {
		g, gs := stateGlyph(st)
		legend = append(legend, gs.Render(g)+dim.Render(" "+st))
	}
	b.WriteString(strings.Join(legend, dim.Render(" · ")) + dim.Render(" · ") + unread.Render("•") + dim.Render(" unread") + "\n")
	if m.msg != "" { // only failures end up here
		b.WriteString(m.msg)
	}
	return b.String()
}

// overCount reports whether screen column x sits on the "(n)" of a group header.
// Layout is: 2-col mark, arrow and space, project name, space, "(n)".
func overCount(r row, x int) bool {
	start := 2 + 2 + utf8.RuneCountInString(r.project) + 1
	end := start + len(fmt.Sprint(r.n)) + 2
	return end <= 2+projW && x >= start && x < end // false when the name got truncated
}

func (m model) countHint(r row) string {
	counts := map[string]int{}
	for _, s := range m.sessions {
		if s.Project() == r.project {
			counts[s.State]++
		}
	}
	parts := []string{fmt.Sprintf("%d Claude sessions", r.n)}
	for _, st := range []string{"blocked", "failed", "working", "idle"} {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
		}
	}
	return strings.Join(append(parts, "click to expand"), " · ")
}

// jump brings the terminal the session runs in to the front. tmux first so the
// right pane is selected, then iTerm2, Ghostty or VS Code so the right window is on top.
// Returns "" on success, otherwise what went wrong.
func jump(s Session) string {
	// The live pid gives the tty as it is now. That beats one a hook saw hours
	// ago, and covers sessions that had no event since wmw learned about Ghostty.
	// The TERM_PROGRAM check keeps a jump from launching Ghostty for iTerm2 users.
	if s.PID != 0 && s.Pane == "" && s.ITerm == "" && s.Term == "" &&
		(s.Ghostty != "" || os.Getenv("TERM_PROGRAM") == "ghostty") {
		if t := ttyOf(s.PID); t != "" {
			s.Ghostty = t
		}
	}
	if s.Pane == "" && s.ITerm == "" && s.Ghostty == "" && s.Term == "" {
		return s.Project() + " was not started in tmux, iTerm2, Ghostty, or VS Code"
	}
	var problems []string
	if s.Pane != "" && os.Getenv("TMUX") != "" {
		exec.Command("tmux", "select-pane", "-t", s.Pane).Run()
		exec.Command("tmux", "select-window", "-t", s.Pane).Run()
		if exec.Command("tmux", "switch-client", "-t", s.Pane).Run() != nil {
			problems = append(problems, "tmux pane gone")
		}
	}
	if s.ITerm != "" {
		cmd := exec.Command("osascript", "-", s.ITerm)
		cmd.Stdin = strings.NewReader(iTermFocus)
		if res, err := cmd.Output(); err != nil || strings.TrimSpace(string(res)) != "ok" {
			problems = append(problems, "iTerm2 session gone")
		}
	}
	if s.Ghostty != "" {
		cmd := exec.Command("osascript", "-", s.Ghostty)
		cmd.Stdin = strings.NewReader(ghosttyFocus)
		if res, err := cmd.Output(); err != nil || strings.TrimSpace(string(res)) != "ok" {
			problems = append(problems, "Ghostty terminal gone")
		}
	}
	if s.Term == "vscode" {
		// -r reuses the window that has the repo open, which is what raises it.
		// The git root matters: pointing -r at a subfolder would add it to the workspace.
		root := s.CWD
		if b, err := exec.Command("git", "-C", s.CWD, "rev-parse", "--show-toplevel").Output(); err == nil {
			root = strings.TrimSpace(string(b))
		}
		if exec.Command("code", "-r", root).Run() != nil {
			problems = append(problems, "code CLI failed")
		}
	}
	if len(problems) == 0 {
		return ""
	}
	return s.Project() + ": " + strings.Join(problems, ", ")
}

const ghosttyFocus = `on run argv
  tell application "Ghostty"
    repeat with t in terminals
      if tty of t is item 1 of argv then
        focus t
        activate
        return "ok"
      end if
    end repeat
  end tell
  return "gone"
end run
`

const iTermFocus = `on run argv
  tell application "iTerm2"
    repeat with w in windows
      repeat with t in tabs of w
        repeat with s in sessions of t
          if id of s is item 1 of argv then
            select s
            select t
            select w
            activate
            return "ok"
          end if
        end repeat
      end repeat
    end repeat
  end tell
  return "gone"
end run
`
