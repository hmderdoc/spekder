package main

import (
	"bufio"
	"fmt"
	"sort"
	"strings"
	"time"

	"spekder/internal/proto"
)

type chatToast struct {
	msg    proto.ChatMessage
	until  time.Time
	seqKey uint32
}

type chatUI struct {
	active     bool
	cmd        bool // compose is a slash-command (/) line, not a chat message
	party      bool // compose is a PARTY message (\), scoped to your party
	transcript bool
	partyView  bool // transcript shows only party messages (| opened it)
	input      string
	seenSeq    uint32
	messages   []proto.ChatMessage
	toasts     []chatToast
	who        string // "WHO:" roster strip for the transcript (see setWho)
}

// setWho rebuilds the transcript's connected-roster line from the server's
// presence list. Anonymous (pre-login) sessions collapse into a single count;
// who_hide_anonymous=true in the ini drops them entirely (a busy login attract
// screen would otherwise pad the list with noise).
func (c *chatUI) setWho(pres []proto.Presence) {
	hideAnon := iniTruthy(loadINI(defaultINIPath())["who_hide_anonymous"])
	var named []string
	anon := 0
	for _, rec := range pres {
		if rec.State == "offline" {
			continue
		}
		if rec.Handle == "" || rec.Handle == "player" { // door32Identity's pre-login default
			anon++
			continue
		}
		label := rec.Handle
		if rec.State != "" {
			label += " (" + rec.State + ")"
		}
		named = append(named, label)
	}
	sort.Strings(named)
	parts := named
	if anon > 0 && !hideAnon {
		parts = append(parts, fmt.Sprintf("%d anonymous", anon))
	}
	if len(parts) == 0 {
		c.who = "WHO: nobody connected"
		return
	}
	c.who = "WHO: " + strings.Join(parts, ", ")
}

func (c *chatUI) ingest(msgs []proto.ChatMessage) {
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Seq < msgs[j].Seq })
	now := time.Now()
	for _, msg := range msgs {
		if msg.Seq == 0 {
			continue
		}
		if msg.Seq > c.seenSeq {
			c.seenSeq = msg.Seq
			until := time.Unix(int64(msg.Time), 0).Add(20 * time.Second)
			if msg.Time == 0 {
				until = now.Add(20 * time.Second)
			}
			if now.Before(until) {
				c.toasts = append(c.toasts, chatToast{msg: msg, until: until, seqKey: msg.Seq})
			}
		}
	}
	c.messages = append(c.messages[:0], msgs...)
	if len(c.messages) > 50 {
		c.messages = c.messages[len(c.messages)-50:]
	}
}

func (c *chatUI) prune() bool {
	now := time.Now()
	n := 0
	for _, t := range c.toasts {
		if now.Before(t.until) {
			c.toasts[n] = t
			n++
		}
	}
	changed := n != len(c.toasts)
	c.toasts = c.toasts[:n]
	return changed
}

func (c *chatUI) appendRune(r rune) {
	if r < 0x20 || r == 0x7f || r == '`' || r == '~' || r == '\\' || r == '|' {
		return
	}
	if c.cmd && c.input == "" && r == '/' {
		return // swallow the echo of the "/" that opened command mode
	}
	if len(c.input) >= 180 {
		return
	}
	c.input += string(r)
}

func (c *chatUI) backspace() {
	if c.input == "" {
		return
	}
	rs := []rune(c.input)
	c.input = string(rs[:len(rs)-1])
}

func (c *chatUI) submit(dropfile string) string {
	text := strings.TrimSpace(c.input)
	wasCmd, wasParty := c.cmd, c.party
	c.input = ""
	c.active, c.cmd, c.party = false, false, false
	if wasCmd {
		return dispatchCommand(text, dropfile)
	}
	if text == "" {
		return ""
	}
	scope := "" // global
	if wasParty {
		if scope = getParty(); scope == "" {
			return "You're not in a party."
		}
	}
	if err := sendChat(dropfile, text, scope); err != nil {
		return "Chat failed: " + err.Error()
	}
	return ""
}

// dispatchCommand runs a slash-command line (the leading "/" already stripped)
// and returns a one-line result to surface to the player. Usable anywhere chat
// compose is - the menu and in a match.
func dispatchCommand(line, dropfile string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "Commands: /kick <player>, /leave"
	}
	_, myHandle := door32Identity(dropfile)
	switch strings.ToLower(fields[0]) {
	case "kick":
		if len(fields) < 2 {
			return "Usage: /kick <player>"
		}
		party := getParty()
		if party == "" {
			return "You're not in a party."
		}
		if party != myHandle { // ownership is by naming: the party carries the owner's handle
			return "Only the owner (" + party + ") can kick."
		}
		target := fields[1]
		if err := sendPartyKick(dropfile, party, target); err != nil {
			return "Kick failed: " + err.Error()
		}
		return "Kicked " + target + " from the party."
	case "leave":
		if getParty() == "" {
			return "You're not in a party."
		}
		setParty("")
		updatePresence(dropfile, "menu", "")
		return "Left the party."
	case "help":
		return "Commands: /kick <player>, /leave"
	}
	return "Unknown command '" + fields[0] + "'. Try /help."
}

func chatLine(m proto.ChatMessage) string {
	if m.Handle == "" { // server-generated notice (e.g. "X voted for MAP"): no prefix
		return m.Text
	}
	return m.Handle + ": " + m.Text
}

// drawChat renders chat (toasts, compose bar, or transcript). mark, if non-nil,
// is called with (row, col, vis) for every cell run drawn, so a masked caller
// (the lobby's rotating-map preview) can skip those cells in its delta blit and
// keep the chat from flickering. The normal game path passes nil.
func drawChat(w *bufio.Writer, cols, rows int, c *chatUI, mark func(row, col, vis int)) {
	c.prune()
	if c.transcript {
		drawChatTranscript(w, cols, rows, c, mark)
		return
	}
	drawChatToasts(w, cols, rows, c, mark)
	if c.active {
		drawChatInput(w, cols, rows, c, mark)
	}
}

func drawChatToasts(w *bufio.Writer, cols, rows int, c *chatUI, mark func(row, col, vis int)) {
	width := cols - 4
	if width > 76 {
		width = 76
	}
	if width < 20 {
		return
	}
	type tline struct{ text, style string }
	var lines []tline
	for _, t := range c.toasts {
		style := "\x1b[1;37;40m" // global: bright white
		if t.msg.Party != "" {
			style = "\x1b[1;95;40m" // party: bright magenta
		}
		for _, wl := range wrapText(chatLine(t.msg), width) {
			lines = append(lines, tline{wl, style})
		}
	}
	maxLines := 5
	if c.active {
		maxLines = 3
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	start := rows - 1 - len(lines)
	if c.active {
		start--
	}
	if start < 1 {
		start = 1
	}
	for i, ln := range lines {
		fmt.Fprintf(w, "\x1b[%d;2H%s%-*s\x1b[0m", start+i, ln.style, width, clip(ln.text, width))
		if mark != nil {
			mark(start+i, 2, width)
		}
	}
}

func drawChatInput(w *bufio.Writer, cols, rows int, c *chatUI, mark func(row, col, vis int)) {
	width := cols - 4
	if width > 76 {
		width = 76
	}
	prefix, color := "` ", "\x1b[1;30;46m" // chat: black on cyan
	switch {
	case c.cmd:
		prefix, color = "/", "\x1b[1;30;42m" // command: black on green
	case c.party:
		prefix, color = "\\ ", "\x1b[1;30;45m" // party: black on magenta
	}
	prompt := prefix + c.input
	if len(prompt) > width-1 {
		prompt = prompt[len(prompt)-(width-1):]
	}
	fmt.Fprintf(w, "\x1b[%d;2H%s%-*s\x1b[0m", rows-1, color, width, clip(prompt+"_", width))
	if mark != nil {
		mark(rows-1, 2, width)
	}
}

func drawChatTranscript(w *bufio.Writer, cols, rows int, c *chatUI, mark func(row, col, vis int)) {
	bw := cols - 8
	if bw > 78 {
		bw = 78
	}
	if bw < 28 {
		bw = cols - 2
	}
	bh := rows - 6
	if bh > 18 {
		bh = 18
	}
	if bh < 8 {
		bh = rows - 2
	}
	left := (cols-bw)/2 + 1
	top := (rows-bh)/2 + 1
	inner := bw - 2
	hline := strings.Repeat(string([]byte{0xC4}), inner)
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;36;40m%s%s%s\x1b[0m", top, left, string([]byte{0xDA}), hline, string([]byte{0xBF}))
	for r := 1; r < bh-1; r++ {
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;36;40m%s\x1b[0;37;40m%-*s\x1b[1;36;40m%s\x1b[0m", top+r, left, string([]byte{0xB3}), inner, "", string([]byte{0xB3}))
	}
	fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;36;40m%s%s%s\x1b[0m", top+bh-1, left, string([]byte{0xC0}), hline, string([]byte{0xD9}))
	title := " CHAT TRANSCRIPT  (~ closes, ` compose) "
	titleColor := "\x1b[1;30;46m"
	if c.partyView {
		title = " PARTY CHAT  (| closes, \\ compose) "
		titleColor = "\x1b[1;30;45m"
	}
	fmt.Fprintf(w, "\x1b[%d;%dH%s%s\x1b[0m", top, left+2, titleColor, clip(title, inner-2))
	row := top + 1
	if c.who != "" { // connected-roster strip: a couple of rows + a divider
		wl := wrapText(c.who, inner-2)
		if len(wl) > 2 {
			wl = wl[:2]
		}
		for _, line := range wl {
			fmt.Fprintf(w, "\x1b[%d;%dH\x1b[0;36;40m%-*s\x1b[0m", row, left+1, inner, clip(" "+line, inner))
			row++
		}
		fmt.Fprintf(w, "\x1b[%d;%dH\x1b[1;36;40m%s%s%s\x1b[0m", row, left, string([]byte{0xC3}), hline, string([]byte{0xB4}))
		row++
	}
	type tline struct{ text, style string }
	var lines []tline
	for _, msg := range c.messages {
		if c.partyView && msg.Party == "" {
			continue // party view: only party-scoped messages
		}
		style := "\x1b[0;37;40m"
		if msg.Party != "" {
			style = "\x1b[1;95;40m" // party message: magenta, even in the global view
		}
		for _, wl := range wrapText(chatLine(msg), inner-2) {
			lines = append(lines, tline{wl, style})
		}
	}
	max := top + bh - 2 - row // leave the bottom inner row for the compose prompt
	if max < 1 {
		max = 1
	}
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	for i, ln := range lines {
		fmt.Fprintf(w, "\x1b[%d;%dH%s%-*s\x1b[0m", row+i, left+1, ln.style, inner, clip(ln.text, inner))
	}
	if c.active {
		prompt, pc := "` "+c.input+"_", "\x1b[1;30;46m"
		if c.party {
			prompt, pc = "\\ "+c.input+"_", "\x1b[1;30;45m"
		}
		fmt.Fprintf(w, "\x1b[%d;%dH%s%-*s\x1b[0m", top+bh-2, left+1, pc, inner, clip(prompt, inner))
	}
	if mark != nil { // the box is opaque: mask the whole rectangle
		for r := 0; r < bh; r++ {
			mark(top+r, left, bw)
		}
	}
}
