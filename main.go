package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	refreshInterval = 3 * time.Second
	msgLimit        = 200
	listWidth       = 34
)

// Railscast (base16) palette — Ryan Bates' classic vim/TextMate theme.
var (
	rcBg        = lipgloss.Color("#2B2B2B")
	rcFg        = lipgloss.Color("#E8E8D3")
	rcDim       = lipgloss.Color("#5F5A60") // comments, timestamps, footer
	rcSelection = lipgloss.Color("#5A647E") // cursor row background (railscast visual-select blue)
	rcBorder    = lipgloss.Color("#4B4B4B") // inactive pane border
	rcGreen     = lipgloss.Color("#A5C261") // active border
	rcYellow    = lipgloss.Color("#FFC66D") // unread badges
	rcBlue      = lipgloss.Color("#6D9CBE") // "me" message header
	rcOrange    = lipgloss.Color("#CC7833") // other-sender message header
	rcRed       = lipgloss.Color("#DA4939") // errors, deleted messages
	rcImage     = lipgloss.Color("#B452CD") // messages that carry an image
)

type tickMsg time.Time

type chatsLoadedMsg struct {
	chats []Chat
	err   error
}

type messagesLoadedMsg struct {
	jid  string
	msgs []Message
	err  error
}

type imageLoadedMsg struct {
	jid   string
	msgID string
	png   []byte
	w, h  int
	err   error
}

type imgEntry struct {
	png     []byte
	err     string
	loading bool
	w, h    int // source pixel dimensions, used to fit the inline preview
}

type model struct {
	chats     []Chat
	cursor    int  // selected row in chat list
	focusList bool // true: keys move chat selection; false: keys scroll messages
	openJID   string
	openMsgs  []Message
	viewport  viewport.Model
	width     int
	height    int
	err       string
	ready     bool
	lastKey   string // tracks "g" for the "gg" (goto top) motion

	selectedMsg  int   // index into openMsgs the user has selected; -1 = none yet
	msgLineStart []int // msgLineStart[i] = first content line of openMsgs[i]; len = len(openMsgs)+1

	imgCache map[string]*imgEntry
}

func initialModel() model {
	return model{focusList: true, ready: false, selectedMsg: -1, imgCache: map[string]*imgEntry{}}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(loadChats(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func loadChats() tea.Cmd {
	return func() tea.Msg {
		chats, err := fetchChats()
		return chatsLoadedMsg{chats: chats, err: err}
	}
}

func loadMessages(jid string) tea.Cmd {
	return func() tea.Msg {
		msgs, err := fetchMessages(jid, msgLimit)
		return messagesLoadedMsg{jid: jid, msgs: msgs, err: err}
	}
}

func loadImage(jid, msgID string) tea.Cmd {
	return func() tea.Msg {
		path, err := downloadMedia(jid, msgID)
		if err != nil {
			return imageLoadedMsg{jid: jid, msgID: msgID, err: err}
		}
		png, w, h, err := loadImagePNG(path)
		return imageLoadedMsg{jid: jid, msgID: msgID, png: png, w: w, h: h, err: err}
	}
}

type chatMarkedReadMsg struct{ err error }

// markChatReadCmd marks jid as read, so opening a chat clears its unread
// badge the way it would in the WhatsApp app itself.
func markChatReadCmd(jid string) tea.Cmd {
	return func() tea.Msg {
		return chatMarkedReadMsg{err: markChatRead(jid)}
	}
}

type enlargeResultMsg struct{ err error }

// enlargeImage downloads the message's full-resolution media (reusing the
// on-disk cache wacli already keeps, so this is cheap even if the small
// inline preview already fetched it) and opens it in a new split pane
// alongside wtui, rather than overlaying wtui's own pane or opening a
// separate window.
func enlargeImage(jid, msgID string) tea.Cmd {
	return func() tea.Msg {
		path, err := downloadMedia(jid, msgID)
		if err != nil {
			return enlargeResultMsg{err: err}
		}
		return enlargeResultMsg{err: openImageSplit(path)}
	}
}

// openImageSplit asks kitty's remote-control protocol to open a new
// horizontally-split window pane running icat and switching focus to it
// immediately, leaving wtui's own pane running (but unfocused) behind it.
// --hold keeps the new pane open (showing the image) until the user closes
// it themselves.
func openImageSplit(path string) error {
	if !kittySupported() {
		return exec.Command("open", path).Start()
	}
	return exec.Command("kitty", "@", "launch",
		"--type=window", "--location=hsplit", "--title=image",
		"kitty", "+kitten", "icat", "--hold", path).Run()
}

// cellAspect approximates a monospace cell's width-to-height ratio (no
// portable way to query the exact ratio from here), used to fit an image
// of pixel size w x h into a maxCols x maxRows terminal-cell box without
// distorting its aspect ratio.
const cellAspect = 0.5 // cell width / cell height

func fitImage(w, h, maxCols, maxRows int) (cols, rows int) {
	if w <= 0 || h <= 0 || maxCols <= 0 || maxRows <= 0 {
		return 0, 0
	}
	cols = maxCols
	rows = int(float64(cols) * (float64(h) / float64(w)) * cellAspect)
	if rows > maxRows {
		rows = maxRows
		cols = int(float64(rows) / cellAspect * (float64(w) / float64(h)))
		if cols > maxCols {
			cols = maxCols
		}
	}
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	return
}

// imageBoxRows is how many lines renderMessages reserves below the
// selected message's "🖼 [image]" marker, so the inline preview has
// somewhere to draw without overlapping the text that follows.
const imageBoxRows = 12

// hasSelectedImage reports whether the currently selected message is a
// live (not revoked/deleted) image — the only case where the inline
// preview is shown at all.
func (m model) hasSelectedImage() bool {
	if m.selectedMsg < 0 || m.selectedMsg >= len(m.openMsgs) {
		return false
	}
	msg := m.openMsgs[m.selectedMsg]
	return msg.MediaType == "image" && !msg.Revoked && !msg.DeletedForMe
}

// loadSelectedImage kicks off a download for the currently selected
// message's image, if it has one and it isn't cached yet. Called after
// anything that can change the selection, so the preview lazily loads just
// the one image it's about to show.
func (m model) loadSelectedImage() tea.Cmd {
	if !kittySupported() || !m.hasSelectedImage() {
		return nil
	}
	msg := m.openMsgs[m.selectedMsg]
	if m.imgCache[msg.MsgID] != nil {
		return nil
	}
	m.imgCache[msg.MsgID] = &imgEntry{loading: true}
	return loadImage(m.openJID, msg.MsgID)
}

// msgPaneWidth is the message pane's content width.
func (m model) msgPaneWidth() int {
	w := m.width - listWidth - 4
	if w < 10 {
		w = 10
	}
	return w
}

// rebuildContent re-renders the open chat's messages, highlighting the
// currently selected message.
func (m *model) rebuildContent() {
	w := m.msgPaneWidth()
	m.viewport.Width = w
	content, lineStart := renderMessages(m.openMsgs, w, m.selectedMsg)
	m.msgLineStart = lineStart
	m.viewport.SetContent(content)
}

// scrollToSelection adjusts the viewport's scroll offset, if needed, so
// the selected message's whole block is visible (pinning to its end if
// the block itself is taller than the viewport).
func (m *model) scrollToSelection() {
	if m.selectedMsg < 0 || m.selectedMsg+1 >= len(m.msgLineStart) {
		return
	}
	start := m.msgLineStart[m.selectedMsg]
	end := m.msgLineStart[m.selectedMsg+1] - 1
	if start < m.viewport.YOffset {
		m.viewport.SetYOffset(start)
	} else if end > m.viewport.YOffset+m.viewport.Height-1 {
		off := end - m.viewport.Height + 1
		if off < 0 {
			off = 0
		}
		m.viewport.SetYOffset(off)
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		vpW := m.msgPaneWidth()
		vpH := m.height - 4
		if vpH < 3 {
			vpH = 3
		}
		if !m.ready {
			m.viewport = viewport.New(vpW, vpH)
			m.ready = true
		} else {
			m.viewport.Width = vpW
			m.viewport.Height = vpH
		}
		return m, m.loadSelectedImage()

	case tickMsg:
		cmds := []tea.Cmd{loadChats(), tick()}
		if m.openJID != "" {
			cmds = append(cmds, loadMessages(m.openJID))
		}
		return m, tea.Batch(cmds...)

	case chatsLoadedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err = ""
		m.chats = msg.chats
		if m.cursor >= len(m.chats) {
			m.cursor = len(m.chats) - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}
		return m, nil

	case messagesLoadedMsg:
		if msg.jid != m.openJID {
			return m, nil // stale response from a chat we've since left
		}
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err = ""
		// Follow the newest message across refreshes only if the user
		// hadn't scrolled back to read older ones.
		wasAtNewest := m.selectedMsg < 0 || m.selectedMsg >= len(m.openMsgs)-1
		m.openMsgs = msg.msgs
		if wasAtNewest || m.selectedMsg >= len(m.openMsgs) {
			m.selectedMsg = len(m.openMsgs) - 1
		}
		m.rebuildContent()
		m.scrollToSelection()
		return m, m.loadSelectedImage()

	case imageLoadedMsg:
		if msg.jid != m.openJID {
			return m, nil // stale: left this chat before the download finished
		}
		e := m.imgCache[msg.msgID]
		if e == nil {
			e = &imgEntry{}
			m.imgCache[msg.msgID] = e
		}
		e.loading = false
		if msg.err != nil {
			e.err = msg.err.Error()
		} else {
			e.png = msg.png
			e.w, e.h = msg.w, msg.h
		}
		return m, nil

	case chatMarkedReadMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		return m, nil

	case enlargeResultMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		return m, nil

	case tea.KeyMsg:
		key := msg.String()
		wasG := m.lastKey == "g"
		m.lastKey = ""

		if key == "g" {
			if wasG {
				if m.focusList {
					m.cursor = 0
				} else if len(m.openMsgs) > 0 {
					m.selectedMsg = 0
					m.rebuildContent()
					m.scrollToSelection()
				}
				return m, m.loadSelectedImage()
			}
			m.lastKey = "g"
			return m, nil
		}

		switch key {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "tab":
			m.focusList = !m.focusList
			return m, nil
		case "G":
			if m.focusList {
				m.cursor = len(m.chats) - 1
			} else if len(m.openMsgs) > 0 {
				m.selectedMsg = len(m.openMsgs) - 1
				m.rebuildContent()
				m.scrollToSelection()
			}
			return m, m.loadSelectedImage()
		case "ctrl+d":
			if m.focusList {
				m.cursor += (m.height - 3) / 2
				if m.cursor > len(m.chats)-1 {
					m.cursor = len(m.chats) - 1
				}
			} else {
				m.viewport.HalfPageDown()
			}
			return m, nil
		case "ctrl+u":
			if m.focusList {
				m.cursor -= (m.height - 3) / 2
				if m.cursor < 0 {
					m.cursor = 0
				}
			} else {
				m.viewport.HalfPageUp()
			}
			return m, nil
		case "up", "k":
			if m.focusList {
				if m.cursor > 0 {
					m.cursor--
				}
				return m, nil
			} else if m.selectedMsg > 0 {
				m.selectedMsg--
				m.rebuildContent()
				m.scrollToSelection()
			}
			return m, m.loadSelectedImage()
		case "down", "j":
			if m.focusList {
				if m.cursor < len(m.chats)-1 {
					m.cursor++
				}
				return m, nil
			} else if m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs)-1 {
				m.selectedMsg++
				m.rebuildContent()
				m.scrollToSelection()
			}
			return m, m.loadSelectedImage()
		case "pgup":
			m.viewport.PageUp()
			return m, nil
		case "pgdown":
			m.viewport.PageDown()
			return m, nil
		case "enter", "l", "right":
			if m.focusList && len(m.chats) > 0 {
				jid := m.chats[m.cursor].JID
				m.openJID = jid
				m.openMsgs = nil
				m.selectedMsg = -1
				m.viewport.SetContent("loading…")
				m.focusList = false
				return m, tea.Batch(loadMessages(jid), markChatReadCmd(jid))
			}
			return m, nil
		case "o":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				sel := m.openMsgs[m.selectedMsg]
				if sel.MediaType == "image" && !sel.Revoked && !sel.DeletedForMe {
					return m, enlargeImage(m.openJID, sel.MsgID)
				}
			}
			return m, nil
		case "h", "left", "esc":
			m.focusList = true
			return m, nil
		}
	}
	return m, nil
}

func (m model) View() string {
	if !m.ready {
		return "loading…"
	}

	listPane := renderChatList(m.chats, m.cursor, m.openJID, m.focusList, listWidth, m.height-3)

	msgPane := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor(!m.focusList)).
		Background(rcBg).
		Width(m.msgPaneWidth()).
		Height(m.height - 4).
		Render(m.headerFor(m.openJID) + "\n" + m.viewport.View())

	body := lipgloss.JoinHorizontal(lipgloss.Top, listPane, msgPane)

	footer := "tab: switch pane  ↑/↓ j/k gg/G: select  ^d/^u: page  enter: open chat  o: open image  esc: back  q: quit"
	if m.err != "" {
		footer = lipgloss.NewStyle().Foreground(rcRed).Render("error: " + m.err)
	} else if m.hasSelectedImage() {
		if e := m.imgCache[m.openMsgs[m.selectedMsg].MsgID]; e != nil && e.err != "" {
			footer = lipgloss.NewStyle().Foreground(rcRed).Render("image preview error: " + e.err)
		}
	}
	footer += m.inlineImageEscapes()
	footerStyle := lipgloss.NewStyle().Foreground(rcDim).Width(m.width)

	return body + "\n" + footerStyle.Render(footer)
}

// inlineImageEscapes returns the kitty graphics escape sequence (invisible,
// zero-width control bytes) that draws the selected message's image into
// the blank rows renderMessages reserved below its "🖼 [image]" marker,
// positioned via an absolute cursor move relative to the viewport's current
// scroll offset. Appending this to the footer (rather than embedding it in
// the pane's own rendered string) means it naturally gets re-sent by
// bubbletea's line-diffing renderer exactly when the selection, scroll
// position, or image changes — the footer's own string differs byte-for-byte
// whenever that happens — and skipped otherwise. It always clears the
// previous image first, so switching away from an image message (or
// scrolling its box off-screen) doesn't leave stale pixels.
func (m model) inlineImageEscapes() string {
	if !kittySupported() {
		return ""
	}
	const imgID = 1
	var b strings.Builder
	b.WriteString(kittyDeleteImage(imgID))

	if !m.hasSelectedImage() || m.selectedMsg+1 >= len(m.msgLineStart) {
		return b.String()
	}
	msg := m.openMsgs[m.selectedMsg]
	e := m.imgCache[msg.MsgID]
	if e == nil || e.png == nil {
		return b.String()
	}
	boxLine := m.msgLineStart[m.selectedMsg] + 2 // header line + marker line
	rel := boxLine - m.viewport.YOffset
	if rel < 0 || rel >= m.viewport.Height {
		return b.String()
	}
	availRows := imageBoxRows
	if rel+availRows > m.viewport.Height {
		availRows = m.viewport.Height - rel
	}
	if availRows < 1 {
		return b.String()
	}
	cols, rows := fitImage(e.w, e.h, m.msgPaneWidth()-2, availRows)
	topRow := 3 + rel        // msgPane border(1) + header(1), then rel lines into the viewport
	leftCol := listWidth + 4 // listPane width+border, then msgPane's own border+inset
	fmt.Fprintf(&b, "\x1b[%d;%dH", topRow, leftCol)
	b.WriteString(kittyShowImage(e.png, imgID, cols, rows))
	return b.String()
}

func (m model) headerFor(jid string) string {
	for _, c := range m.chats {
		if c.JID == jid {
			return lipgloss.NewStyle().Bold(true).Foreground(rcFg).Render(c.Name)
		}
	}
	if jid == "" {
		return lipgloss.NewStyle().Foreground(rcDim).Render("select a chat →")
	}
	return jid
}

func borderColor(active bool) lipgloss.Color {
	if active {
		return rcGreen
	}
	return rcBorder
}

func renderChatList(chats []Chat, cursor int, openJID string, focused bool, width, height int) string {
	var b strings.Builder
	nameW := width - 10
	if nameW < 4 {
		nameW = 4
	}
	start := 0
	if cursor >= height {
		start = cursor - height + 1
	}
	end := start + height
	if end > len(chats) {
		end = len(chats)
	}
	for i := start; i < end; i++ {
		c := chats[i]
		name := truncate(c.Name, nameW)
		badge := ""
		if c.UnreadCount > 0 {
			badge = fmt.Sprintf(" (%d)", c.UnreadCount)
		}
		icon := "👤"
		if c.Kind == "group" {
			icon = "👥"
		} else if c.Kind == "newsletter" || c.Kind == "channel" {
			icon = "📢"
		}
		line := fmt.Sprintf("%s %-*s%s", icon, nameW, name, badge)

		style := lipgloss.NewStyle().Foreground(rcFg)
		if c.JID == openJID {
			style = style.Bold(true)
		}
		if c.UnreadCount > 0 {
			style = style.Foreground(rcYellow)
		}
		if i == cursor {
			style = style.Background(rcSelection)
		}
		b.WriteString(style.Render(line))
		b.WriteString("\n")
	}
	content := b.String()
	border := lipgloss.RoundedBorder()
	return lipgloss.NewStyle().
		Border(border).
		BorderForeground(borderColor(focused)).
		Background(rcBg).
		Width(width).
		Height(height).
		Render(content)
}

// renderMessages builds the viewport content. selectedIdx's message, if
// any, is drawn on a highlighted background across its full width, and is
// the only message whose image (if it has one) appears in the preview
// pane — see imagePaneEscapes. lineStart[i] is the first content line of
// msgs[i] (lineStart[len(msgs)] is the total line count), used to scroll a
// selection fully into view.
func renderMessages(msgs []Message, width int, selectedIdx int) (string, []int) {
	if len(msgs) == 0 {
		return lipgloss.NewStyle().Faint(true).Render("no messages"), nil
	}
	var b strings.Builder
	lineStart := make([]int, 0, len(msgs)+1)
	line := 0

	writeLine := func(s string) {
		b.WriteString(s)
		b.WriteString("\n")
		line++
	}

	// row renders one line with fg (and, when selected, a full-width bg)
	// set in a single Render call, so a later nested reset can never
	// clobber the highlight the way concatenating separately-styled,
	// separately-reset spans would.
	row := func(s string, fg lipgloss.Color, bold, selected bool) string {
		st := lipgloss.NewStyle().Foreground(fg).Width(width)
		if bold {
			st = st.Bold(true)
		}
		if selected {
			st = st.Background(rcSelection)
		}
		return st.Render(s)
	}

	for i, msg := range msgs {
		lineStart = append(lineStart, line)
		selected := i == selectedIdx

		ts, _ := time.Parse(time.RFC3339, msg.Timestamp)
		when := msg.Timestamp
		if !ts.IsZero() {
			when = ts.Local().Format("Jan 2 15:04")
		}
		who := msg.SenderName
		if msg.FromMe {
			who = "me"
		}
		headerColor := rcOrange
		if msg.FromMe {
			headerColor = rcBlue
		}
		if selected {
			writeLine(row(who+"  "+when, headerColor, true, true))
		} else {
			writeLine(lipgloss.NewStyle().Bold(true).Foreground(headerColor).Render(who) +
				lipgloss.NewStyle().Foreground(rcDim).Render("  "+when))
		}

		isLiveImage := msg.MediaType == "image" && !msg.Revoked && !msg.DeletedForMe
		if isLiveImage {
			writeLine(row("🖼 [image]", rcImage, true, selected))
			if selected {
				// Reserve blank rows for the inline preview drawn by
				// inlineImageEscapes, so the text below doesn't overlap it.
				for range imageBoxRows {
					writeLine(row("", rcFg, false, true))
				}
			}
		}

		text := msg.DisplayText
		textColor := rcFg
		if msg.Revoked {
			text, textColor = "[deleted]", rcRed
		} else if msg.DeletedForMe {
			text, textColor = "[deleted for me]", rcRed
		} else if text == "" && msg.MediaType != "" {
			text = "[" + msg.MediaType + "]"
		}
		if text != "" {
			for _, l := range wrapPlain(collapseBlankLines(text), width) {
				writeLine(row(l, textColor, false, selected))
			}
		}
		writeLine(row("", rcFg, false, selected))
	}
	lineStart = append(lineStart, line)
	return b.String(), lineStart
}

// collapseBlankLines drops empty lines from a message's text (wacli's
// DisplayText sometimes embeds blank-line separators from quote/reply
// formatting), so a single message's block doesn't show internal gaps that
// make it look like more than one message got highlighted.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// wrapPlain word-wraps plain text to width, returning the resulting lines.
// Kept separate from styling so callers can apply a single Foreground +
// Background + Width style per resulting line (see renderMessages' row).
func wrapPlain(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	return strings.Split(lipgloss.NewStyle().Width(width).Render(s), "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
