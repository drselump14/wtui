package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	refreshInterval = 3 * time.Second
	msgLimit        = 200
	listWidth       = 26
)

// Railscast (base16) palette — Ryan Bates' classic vim/TextMate theme.
var (
	rcBg           = lipgloss.Color("#2B2B2B")
	rcFg           = lipgloss.Color("#E8E8D3")
	rcDim          = lipgloss.Color("#5F5A60") // comments, timestamps, footer
	rcSelection    = lipgloss.Color("#5A647E") // chat-list cursor row background (railscast visual-select blue)
	rcMsgSelection = lipgloss.Color("#363A40") // selected message row background
	rcBorder       = lipgloss.Color("#4B4B4B") // inactive pane border
	rcGreen        = lipgloss.Color("#A5C261") // active border
	rcYellow       = lipgloss.Color("#FFC66D") // unread badges
	rcBlue         = lipgloss.Color("#6D9CBE") // "me" message header
	rcOrange       = lipgloss.Color("#CC7833") // other-sender message header
	rcRed          = lipgloss.Color("#DA4939") // errors, deleted messages
	rcImage        = lipgloss.Color("#B452CD") // messages that carry an image
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

// previewLoadMsg fires shortly after the list cursor lands on a chat; the
// chat's messages load only if the cursor is still there.
type previewLoadMsg struct{ jid string }

// previewDelay is how long the list cursor must rest on a chat before its
// messages load, so holding j/k doesn't start one wacli run per row.
const previewDelay = 150 * time.Millisecond

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
	showHelp  bool   // true while the full-screen "?" keybinding help is shown

	selectedMsg  int   // index into openMsgs the user has selected; -1 = none yet
	msgLineStart []int // msgLineStart[i] = first content line of openMsgs[i]; len = len(openMsgs)+1

	imgCache map[string]*imgEntry

	composing    bool           // true while the send box is focused and capturing input
	pending      []pendingSend  // outgoing messages not yet confirmed by a reload (all chats), in send order
	pendingSeq   int            // counter for pending placeholders' local MsgIDs
	sendTicking  bool           // true while the sending spinner's tick loop is running
	sendFrame    int            // spinner frame for in-flight placeholders
	serverMsgs   []Message      // open chat's messages as last loaded from wacli; openMsgs adds placeholders
	input        textarea.Model // multi-line send box
	replyToID    string         // MsgID being replied to via "r"; "" means a plain send
	replyPreview string         // short text shown above the send box while replying

	searching  bool // true while the chat list's "ctrl+/" (ctrl+_) filter bar is focused
	search     textinput.Model
	chatFilter string // live filter text; "" means no filter (show all chats)

	msgSearching bool   // true while the open chat's "/" search bar is focused
	msgQuery     string // live query text; jumps the selection to the nearest match

	// unread holds MsgIDs of incoming messages that arrived unread: the
	// chat's last openUnread incoming messages when it was opened, plus
	// any new incoming ones that show up while it stays open. Cleared on
	// switching chats. wacli has no per-message read flag, so this is
	// derived from the chat's unread count.
	unread     map[string]bool
	openUnread int // chat's unread count when opened; consumed by the first load

	videoLoading bool   // true while "o" is downloading media to open
	loadingLabel string // what is downloading, shown next to the spinner
	videoFrame   int    // spinner frame for the downloading indicator
}

func initialModel() model {
	ti := textarea.New()
	ti.Placeholder = "message…  (alt+enter / ctrl+j: new line)"
	ti.CharLimit = 4096
	ti.ShowLineNumbers = false
	ti.SetHeight(composeRows)
	ti.SetPromptFunc(2, func(line int) string {
		if line == 0 {
			return "› "
		}
		return "  "
	})
	// enter sends (handled in Update); these insert a line break instead.
	ti.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ti.FocusedStyle.CursorLine = lipgloss.NewStyle() // no highlighted cursor row
	ti.FocusedStyle.Base = lipgloss.NewStyle()
	ti.BlurredStyle.Base = lipgloss.NewStyle()

	search := textinput.New()
	search.Prompt = "/ "
	search.Placeholder = "search chats…"
	search.CharLimit = 200

	return model{
		focusList:   false,
		ready:       false,
		selectedMsg: -1,
		imgCache:    map[string]*imgEntry{},
		input:       ti,
		search:      search,
	}
}

// visibleChats returns m.chats narrowed to the live chat-list filter, or
// m.chats itself when there's no filter active.
func (m model) visibleChats() []Chat {
	if m.chatFilter == "" {
		return m.chats
	}
	q := strings.ToLower(m.chatFilter)
	out := make([]Chat, 0, len(m.chats))
	for _, c := range m.chats {
		if strings.Contains(strings.ToLower(c.Name), q) {
			out = append(out, c)
		}
	}
	return out
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

func loadImage(jid, msgID, mediaType string) tea.Cmd {
	return func() tea.Msg {
		path, err := downloadMedia(jid, msgID)
		if err != nil {
			return imageLoadedMsg{jid: jid, msgID: msgID, err: err}
		}
		if mediaType == "gif" {
			framePath, err := extractVideoFrame(path)
			if err != nil {
				return imageLoadedMsg{jid: jid, msgID: msgID, err: err}
			}
			defer os.Remove(framePath)
			path = framePath
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

// replyPreviewFor returns a short, single-line preview of msg's text for
// display above the send box while replying to it.
func replyPreviewFor(msg Message) string {
	text := msg.DisplayText
	if msg.Revoked {
		text = "[deleted]"
	} else if msg.DeletedForMe {
		text = "[deleted for me]"
	} else if text == "" && msg.MediaType != "" {
		text = "[" + msg.MediaType + "]"
	}
	text = flattenPreview(text)
	const maxLen = 60
	if len(text) > maxLen {
		text = text[:maxLen-1] + "…"
	}
	who := msg.SenderName
	if msg.FromMe {
		who = "me"
	}
	return who + ": " + text
}

type editorDoneMsg struct {
	text string
	err  error
}

// openEditorCmd suspends the TUI (exactly as it would for an editor opened
// via $EDITOR by, say, git commit) to let the user draft a longer or
// multi-line message in their own editor, seeded with the current draft.
// $EDITOR may itself contain arguments (e.g. "code --wait"); strings.Fields
// splits those off rather than invoking a shell, so there's no quoting to
// get wrong.
func openEditorCmd(seed string) tea.Cmd {
	f, err := os.CreateTemp("", "wtui-draft-*.txt")
	if err != nil {
		return func() tea.Msg { return editorDoneMsg{err: err} }
	}
	path := f.Name()
	_, werr := f.WriteString(seed)
	f.Close()
	if werr != nil {
		os.Remove(path)
		return func() tea.Msg { return editorDoneMsg{err: werr} }
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	cmd := exec.Command(parts[0], append(parts[1:], path)...)

	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer os.Remove(path)
		if err != nil {
			return editorDoneMsg{err: err}
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return editorDoneMsg{err: rerr}
		}
		return editorDoneMsg{text: strings.TrimRight(string(data), "\n")}
	})
}

// composeRows is the send box's height in text rows.
const composeRows = 4

// flattenPreview collapses multi-line text onto one line (used for the
// reply banner), marking the line breaks.
func flattenPreview(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
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
// The pane stays open (showing the image) until the user presses q.
func openImageSplit(path string) error {
	if !kittySupported() {
		return exec.Command("open", path).Start()
	}
	// icat draws the image and exits; the image stays on screen while the
	// shell waits for q (or Q) to close the pane. Other keys are ignored.
	return exec.Command("kitty", "@", "launch",
		"--type=window", "--location=hsplit", "--title=image",
		"bash", "-c", `kitty +kitten icat "$1" && printf '\n  q: close' && `+
			`while IFS= read -rsn1 k; do [[ $k == [qQ] ]] && break; done`,
		"bash", path).Run()
}

// isPlayable reports whether msg carries media that should be handed to a
// video player rather than rendered as an inline image.
func isPlayable(msg Message) bool {
	return msg.MediaType == "video" || msg.MediaType == "gif"
}

type videoResultMsg struct{ err error }

// openSelected is the universal "o" action for the selected message:
// images and stickers open in a kitty split, videos play in a kitty split,
// other media (documents, audio, …) open in their default macOS app, and
// a text message with a link opens the first link in the browser.
func (m model) openSelected() (tea.Model, tea.Cmd) {
	sel := m.openMsgs[m.selectedMsg]
	switch {
	case isPreviewable(sel):
		return m, enlargeImage(m.openJID, sel.MsgID)
	case sel.MediaType != "" && !sel.Revoked && !sel.DeletedForMe:
		if m.videoLoading {
			return m, nil
		}
		m.videoLoading, m.err = true, ""
		m.loadingLabel = sel.MediaType
		open := openMedia
		if isPlayable(sel) {
			open = playVideo
		}
		return m, tea.Batch(open(m.openJID, sel.MsgID), videoTick())
	}
	if u := firstURL(sel.DisplayText); u != "" {
		if isYouTube(u) && canStreamVideo() {
			// mpv streams it (via yt-dlp) in a kitty split, like a video
			// message; its OSD shows buffering progress.
			return m, func() tea.Msg { return enlargeResultMsg{err: openVideo(u)} }
		}
		return m, func() tea.Msg {
			return enlargeResultMsg{err: exec.Command("open", u).Start()}
		}
	}
	m.err = "nothing to open for this message"
	return m, nil
}

// isYouTube reports whether u points at a YouTube video or short.
func isYouTube(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	host = strings.TrimPrefix(host, "m.")
	switch host {
	case "youtu.be":
		return len(parsed.Path) > 1
	case "youtube.com", "music.youtube.com", "youtube-nocookie.com":
		return parsed.Query().Get("v") != "" ||
			strings.HasPrefix(parsed.Path, "/shorts/") ||
			strings.HasPrefix(parsed.Path, "/live/") ||
			strings.HasPrefix(parsed.Path, "/embed/")
	}
	return false
}

// canStreamVideo reports whether mpv can play a YouTube URL: it needs
// yt-dlp (or youtube-dl) to resolve the stream. Without them, links open
// in the browser instead.
func canStreamVideo() bool {
	if _, err := exec.LookPath("mpv"); err != nil {
		return false
	}
	for _, dl := range []string{"yt-dlp", "youtube-dl"} {
		if _, err := exec.LookPath(dl); err == nil {
			return true
		}
	}
	return false
}

// openMedia downloads a message's media and opens it with the default
// macOS app for its type (Preview for PDFs, Music for audio, …).
func openMedia(jid, msgID string) tea.Cmd {
	return func() tea.Msg {
		path, err := downloadMedia(jid, msgID)
		if err != nil {
			return videoResultMsg{err: err}
		}
		return videoResultMsg{err: exec.Command("open", path).Start()}
	}
}

var urlPattern = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"']+`)

// firstURL returns the first http(s) or www. link in text, without trailing
// punctuation that is usually sentence text rather than part of the link.
func firstURL(text string) string {
	u := urlPattern.FindString(text)
	u = strings.TrimRight(u, ".,;:!?)]}>*_~")
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(u), "http") {
		u = "https://" + u
	}
	return u
}

type videoTickMsg struct{}

// videoTick drives the footer spinner while a video is downloading.
func videoTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return videoTickMsg{} })
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// playVideo downloads the message's media (wacli caches it on disk) and
// then plays it, in a kitty split pane when possible.
func playVideo(jid, msgID string) tea.Cmd {
	return func() tea.Msg {
		path, err := downloadMedia(jid, msgID)
		if err != nil {
			return videoResultMsg{err: err}
		}
		return videoResultMsg{err: openVideo(path)}
	}
}

// openVideo plays path in a new kitty split pane (like the image viewer),
// rendering frames with mpv's kitty graphics output. The pane prints a
// "starting player" line until mpv draws its first frame, and mpv's OSD
// shows a buffering percentage whenever playback stalls on the cache.
// Outside kitty it falls back to a regular mpv window or the OS default.
func openVideo(path string) error {
	mpv, mpvErr := exec.LookPath("mpv")
	if kittySupported() && mpvErr == nil {
		return exec.Command("kitty", "@", "launch",
			"--type=window", "--location=hsplit", "--title=video",
			"sh", "-c", `printf '\n  ▶ starting player…\n'; exec "$@"`, "sh",
			mpv, "--vo=kitty", "--really-quiet",
			"--osd-level=3",
			// cap YouTube streams: kitty graphics output is CPU-bound
			"--ytdl-format=bestvideo[height<=720]+bestaudio/best[height<=720]/best",
			"--osd-msg3=${?paused-for-cache==yes:buffering ${cache-buffering-state}%  }${time-pos} / ${duration}",
			path).Run()
	}
	if mpvErr == nil {
		cmd := exec.Command(mpv, "--no-terminal", "--force-window=immediate", path)
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait() // reap the child when it exits
		return nil
	}
	return exec.Command("open", path).Start()
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

// isPreviewable reports whether msg is a live (not revoked/deleted) image
// or sticker — the kinds of media the kitty split-pane "o" viewer knows
// how to render directly, without handing off to mpv.
func isPreviewable(msg Message) bool {
	if msg.Revoked || msg.DeletedForMe {
		return false
	}
	return msg.MediaType == "image" || msg.MediaType == "sticker"
}

// autoPreviewable reports whether msg should get an automatic inline
// preview as soon as it's selected. Images and stickers always qualify;
// "gif" messages (really short looping videos) qualify too, as long as
// ffmpeg is around to pull a still frame out of them.
func autoPreviewable(msg Message) bool {
	if isPreviewable(msg) {
		return true
	}
	return msg.MediaType == "gif" && !msg.Revoked && !msg.DeletedForMe && canExtractGifFrame()
}

// hasSelectedImage reports whether the currently selected message gets
// an automatic inline preview — the only case where one is shown at all.
func (m model) hasSelectedImage() bool {
	if m.selectedMsg < 0 || m.selectedMsg >= len(m.openMsgs) {
		return false
	}
	return autoPreviewable(m.openMsgs[m.selectedMsg])
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
	return loadImage(m.openJID, msg.MsgID, msg.MediaType)
}

// msgPaneWidth is the message pane's content width.
func (m model) msgPaneWidth() int {
	w := m.width - listWidth - 4
	if w < 10 {
		w = 10
	}
	return w
}

// markUnread updates m.unread before m.openMsgs is replaced by msgs. On
// the first load after opening a chat it marks the last m.openUnread
// incoming messages; on later reloads it marks incoming messages that
// weren't in the previous list.
func (m *model) markUnread(msgs []Message) {
	if m.unread == nil {
		m.unread = map[string]bool{}
	}
	if m.openMsgs == nil {
		n := m.openUnread
		m.openUnread = 0
		for i := len(msgs) - 1; i >= 0 && n > 0; i-- {
			if !msgs[i].FromMe {
				m.unread[msgs[i].MsgID] = true
				n--
			}
		}
		return
	}
	seen := make(map[string]bool, len(m.openMsgs))
	for _, old := range m.openMsgs {
		seen[old.MsgID] = true
	}
	for _, nm := range msgs {
		if !nm.FromMe && !seen[nm.MsgID] {
			m.unread[nm.MsgID] = true
		}
	}
}

// rebuildContent re-renders the open chat's messages, highlighting the
// currently selected message.
func (m *model) rebuildContent() {
	w := m.msgPaneWidth()
	m.viewport.Width = w
	content, lineStart := renderMessages(m.openMsgs, w, m.selectedMsg, m.unread, m.sendStatusLabels())
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

// messageMatches returns the indices of open-chat messages whose
// DisplayText contains m.msgQuery, case-insensitively, in list order.
func (m model) messageMatches() []int {
	if m.msgQuery == "" {
		return nil
	}
	q := strings.ToLower(m.msgQuery)
	var out []int
	for i, msg := range m.openMsgs {
		if strings.Contains(strings.ToLower(msg.DisplayText), q) {
			out = append(out, i)
		}
	}
	return out
}

// jumpToMessageMatch moves the selection to the nearest message (starting
// at fromIdx and searching forward, wrapping around) whose DisplayText
// contains query, case-insensitively. Reports whether a match was found.
func (m *model) jumpToMessageMatch(fromIdx int, query string) bool {
	return m.stepMessageMatch(fromIdx, query, 1)
}

// nextMessageMatch moves the selection to the next (dir 1) or previous
// (dir -1) match of m.msgQuery after the current selection, wrapping.
func (m *model) nextMessageMatch(dir int) bool {
	from := m.selectedMsg + dir
	if m.selectedMsg < 0 {
		from = 0
	}
	return m.stepMessageMatch(from, m.msgQuery, dir)
}

// stepMessageMatch is jumpToMessageMatch with a direction: dir 1 scans
// forward from fromIdx, dir -1 scans backward; both wrap around.
func (m *model) stepMessageMatch(fromIdx int, query string, dir int) bool {
	if query == "" || len(m.openMsgs) == 0 {
		return false
	}
	q := strings.ToLower(query)
	n := len(m.openMsgs)
	for i := 0; i < n; i++ {
		idx := ((fromIdx+dir*i)%n + n) % n
		if strings.Contains(strings.ToLower(m.openMsgs[idx].DisplayText), q) {
			m.selectedMsg = idx
			m.rebuildContent()
			m.scrollToSelection()
			return true
		}
	}
	return false
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	firstLoad := false
	if lm, ok := msg.(messagesLoadedMsg); ok && lm.jid == m.openJID && m.openMsgs == nil {
		firstLoad = true
	}
	oldCursor, oldFilter := m.cursor, m.chatFilter
	next, cmd := m.update(msg)
	nm := next.(model)
	// Moving the chat-list cursor previews that chat in the message pane
	// (focus stays on the list and the chat isn't marked read). The first
	// chat list load previews the top chat the same way.
	_, chatsLoaded := msg.(chatsLoadedMsg)
	if (nm.focusList && (nm.cursor != oldCursor || nm.chatFilter != oldFilter)) ||
		(chatsLoaded && nm.openJID == "") {
		cmd = tea.Batch(cmd, nm.previewCursor())
	}
	// The message selected in the message pane counts as read, so its
	// unread dot clears as soon as the user lands on it: after any key
	// that leaves the message pane focused (moving with j/k, tabbing into
	// it), and on a chat's first load, which auto-selects the newest
	// message. Later refreshes that auto-follow the newest message don't
	// count, so messages arriving while you watch keep their dot.
	_, isKey := msg.(tea.KeyMsg)
	if (isKey || firstLoad) && !nm.focusList && !nm.composing && !nm.searching &&
		nm.selectedMsg >= 0 && nm.selectedMsg < len(nm.openMsgs) {
		if id := nm.openMsgs[nm.selectedMsg].MsgID; nm.unread[id] {
			delete(nm.unread, id)
			nm.rebuildContent()
		}
	}
	return nm, cmd
}

// previewCursor switches the message pane to the chat under the list
// cursor, deferring the load by previewDelay.
func (m *model) previewCursor() tea.Cmd {
	vis := m.visibleChats()
	if m.cursor < 0 || m.cursor >= len(vis) || vis[m.cursor].JID == m.openJID {
		return nil
	}
	m.switchChat(vis[m.cursor])
	jid := m.openJID
	return tea.Tick(previewDelay, func(time.Time) tea.Msg { return previewLoadMsg{jid} })
}

// switchChat makes c the open chat, with its messages not yet loaded.
func (m *model) switchChat(c Chat) {
	m.openJID = c.JID
	m.openMsgs = nil
	m.serverMsgs = nil
	m.msgQuery = ""
	m.unread = map[string]bool{}
	m.openUnread = c.UnreadCount
	m.selectedMsg = -1
	m.viewport.SetContent("loading…")
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		vpW := m.msgPaneWidth()
		vpH := m.height - 6 - composeRows // header + divider + reply-context line + send box rows, plus borders/footer
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
		m.input.SetWidth(vpW)
		m.search.Width = listWidth - len(m.search.Prompt) - 1
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
		if n := len(m.visibleChats()); m.cursor >= n {
			m.cursor = n - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}
		return m, nil

	case previewLoadMsg:
		if msg.jid == m.openJID && m.openMsgs == nil {
			return m, loadMessages(msg.jid)
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
		m.markUnread(msg.msgs)
		if msg.msgs == nil {
			msg.msgs = []Message{}
		}
		m.serverMsgs = msg.msgs
		m.dropDoneSends(msg.jid, msg.msgs)
		m.mergePending()
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

	case pendingSentMsg:
		return m, m.finishSend(msg)

	case sendTickMsg:
		if !m.sendsActive() {
			m.sendTicking = false
			return m, nil
		}
		m.sendFrame = (m.sendFrame + 1) % len(spinnerFrames)
		m.rebuildContent()
		return m, sendTick()

	case editorDoneMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, m.input.Focus()
		}
		m.err = ""
		m.input.SetValue(msg.text)
		m.input.CursorEnd()
		return m, m.input.Focus()

	case videoResultMsg:
		m.videoLoading = false
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		return m, nil

	case videoTickMsg:
		if !m.videoLoading {
			return m, nil
		}
		m.videoFrame = (m.videoFrame + 1) % len(spinnerFrames)
		return m, videoTick()

	case enlargeResultMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		return m, nil

	case tea.KeyMsg:
		// An error popup swallows the next key to dismiss it.
		if m.err != "" {
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			m.err = ""
			return m, nil
		}
		if m.showHelp {
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			m.showHelp = false
			return m, nil
		}
		if m.composing {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.composing = false
				m.replyToID = ""
				m.replyPreview = ""
				m.input.Blur()
				m.input.Reset()
				return m, nil
			case "ctrl+g":
				return m, openEditorCmd(m.input.Value())
			case "enter":
				text := strings.TrimSpace(m.input.Value())
				if text == "" || m.openJID == "" {
					return m, nil
				}
				replyTo := m.replyToID
				m.input.Reset()
				m.replyToID = ""
				m.replyPreview = ""
				// Stay in compose mode so the next message can be typed
				// right away while this one sends in the background.
				return m, m.queueSend(m.openJID, text, replyTo)
			default:
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			}
		}

		if m.searching {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc", "ctrl+/", "ctrl+_":
				// ctrl+/ toggles the chat search: pressing it again while
				// the bar is open closes it, same as esc.
				m.searching = false
				m.chatFilter = ""
				m.search.Blur()
				m.search.Reset()
				m.cursor = 0
				return m, nil
			case "enter":
				// Commit the filter and stop typing — the (still filtered)
				// chat list stays navigable with ctrl+n/ctrl+p.
				m.searching = false
				m.search.Blur()
				return m, nil
			case "up", "ctrl+p":
				// Step through the filtered chats, previewing each in the
				// message pane (without marking it read) while the search
				// bar keeps focus.
				if m.cursor > 0 {
					m.cursor--
				}
				return m, m.previewCursor()
			case "down", "ctrl+n":
				if m.cursor < len(m.visibleChats())-1 {
					m.cursor++
				}
				return m, m.previewCursor()
			default:
				old := m.search.Value()
				var cmd tea.Cmd
				m.search, cmd = m.search.Update(msg)
				m.chatFilter = m.search.Value()
				if m.chatFilter != old {
					m.cursor = 0
				}
				return m, cmd
			}
		}

		if m.msgSearching {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.msgSearching = false
				m.msgQuery = ""
				m.search.Blur()
				m.search.Reset()
				return m, nil
			case "enter":
				m.msgSearching = false
				m.search.Blur()
				if len(m.messageMatches()) == 0 {
					m.msgQuery = ""
				}
				return m, m.loadSelectedImage()
			case "ctrl+n", "down":
				m.nextMessageMatch(1)
				return m, nil
			case "ctrl+p", "up":
				m.nextMessageMatch(-1)
				return m, nil
			default:
				old := m.search.Value()
				var cmd tea.Cmd
				m.search, cmd = m.search.Update(msg)
				m.msgQuery = m.search.Value()
				if m.msgQuery != old {
					from := m.selectedMsg
					if from < 0 {
						from = 0
					}
					m.jumpToMessageMatch(from, m.msgQuery)
				}
				return m, cmd
			}
		}

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
			// Focus never moves to the chat list; it's navigated with
			// ctrl+n/ctrl+p instead and the message pane stays focused.
			return m, nil
		case "G":
			if m.focusList {
				m.cursor = len(m.visibleChats()) - 1
			} else if len(m.openMsgs) > 0 {
				m.selectedMsg = len(m.openMsgs) - 1
				m.rebuildContent()
				m.scrollToSelection()
			}
			return m, m.loadSelectedImage()
		case "ctrl+d":
			if m.focusList {
				m.cursor += (m.height - 3) / 2
				if n := len(m.visibleChats()); m.cursor > n-1 {
					m.cursor = n - 1
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
		case "up":
			if m.focusList {
				if m.cursor > 0 {
					m.cursor--
				}
				return m, nil
			}
			if m.selectedMsg > 0 {
				m.selectedMsg--
				m.rebuildContent()
				m.scrollToSelection()
			}
			return m, m.loadSelectedImage()
		case "k":
			// Unlike the arrow keys, j/k only move the message selection —
			// the chat list is navigated with ctrl+n/ctrl+p instead.
			if m.focusList {
				return m, nil
			}
			if m.selectedMsg > 0 {
				m.selectedMsg--
				m.rebuildContent()
				m.scrollToSelection()
			}
			return m, m.loadSelectedImage()
		case "down":
			if m.focusList {
				if m.cursor < len(m.visibleChats())-1 {
					m.cursor++
				}
				return m, nil
			}
			if m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs)-1 {
				m.selectedMsg++
				m.rebuildContent()
				m.scrollToSelection()
			}
			return m, m.loadSelectedImage()
		case "j":
			if m.focusList {
				return m, nil
			}
			if m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs)-1 {
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
		case "/":
			if m.openJID == "" || len(m.openMsgs) == 0 {
				return m, nil
			}
			m.msgSearching = true
			m.msgQuery = ""
			m.search.Prompt = "/ "
			m.search.Placeholder = "search messages…"
			m.search.Reset()
			return m, m.search.Focus()
		case "ctrl+/", "ctrl+_":
			m.searching = true
			m.chatFilter = ""
			m.cursor = 0
			m.search.Prompt = "/ "
			m.search.Placeholder = "search chats…"
			m.search.Reset()
			return m, m.search.Focus()
		case "enter", "l", "right":
			if m.focusList {
				vis := m.visibleChats()
				if m.cursor >= 0 && m.cursor < len(vis) {
					c := vis[m.cursor]
					m.focusList = false
					cmds := []tea.Cmd{markChatReadCmd(c.JID)}
					// Usually already previewed; reload only if it isn't
					// showing yet.
					if c.JID != m.openJID {
						m.switchChat(c)
					}
					if m.openMsgs == nil {
						cmds = append(cmds, loadMessages(c.JID))
					}
					return m, tea.Batch(cmds...)
				}
			}
			return m, nil
		case "o":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				return m.openSelected()
			}
			return m, nil
		case "i":
			if !m.focusList && m.openJID != "" {
				m.composing = true
				return m, m.input.Focus()
			}
			return m, nil
		case "r":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				sel := m.openMsgs[m.selectedMsg]
				if isPendingID(sel.MsgID) {
					m.err = "can't reply to a message that hasn't been sent yet"
					return m, nil
				}
				m.composing = true
				m.replyToID = sel.MsgID
				m.replyPreview = replyPreviewFor(sel)
				return m, m.input.Focus()
			}
			return m, nil
		case "R", "x":
			if m.focusList || m.selectedMsg < 0 || m.selectedMsg >= len(m.openMsgs) {
				return m, nil
			}
			id := m.openMsgs[m.selectedMsg].MsgID
			var cmd tea.Cmd
			if key == "R" {
				cmd = m.retrySend(id)
			} else if !m.dropFailedSend(id) {
				return m, nil
			}
			m.mergePending()
			m.rebuildContent()
			if cmd != nil && !m.sendTicking {
				m.sendTicking = true
				cmd = tea.Batch(cmd, sendTick())
			}
			return m, cmd
		case "ctrl+r":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				sel := m.openMsgs[m.selectedMsg]
				if sel.QuotedMsgID != "" {
					for i, msg := range m.openMsgs {
						if msg.MsgID == sel.QuotedMsgID {
							m.selectedMsg = i
							m.rebuildContent()
							m.scrollToSelection()
							break
						}
					}
				}
			}
			return m, m.loadSelectedImage()
		case "ctrl+n", "ctrl+p":
			vis := m.visibleChats()
			if len(vis) == 0 {
				return m, nil
			}
			if key == "ctrl+n" {
				if m.cursor < len(vis)-1 {
					m.cursor++
				}
			} else if m.cursor > 0 {
				m.cursor--
			}
			c := vis[m.cursor]
			m.focusList = false
			cmds := []tea.Cmd{markChatReadCmd(c.JID)}
			if c.JID != m.openJID {
				m.switchChat(c)
			}
			cmds = append(cmds, loadMessages(c.JID))
			return m, tea.Batch(cmds...)
		case "n":
			if m.msgQuery == "" {
				return m, nil
			}
			m.nextMessageMatch(1)
			return m, m.loadSelectedImage()
		case "p", "N":
			if m.msgQuery == "" {
				return m, nil
			}
			m.nextMessageMatch(-1)
			return m, m.loadSelectedImage()
		case "h", "left", "esc":
			if m.msgQuery != "" {
				m.msgQuery = ""
				m.search.Reset()
				return m, nil
			}
			if m.chatFilter != "" {
				m.chatFilter = ""
				m.cursor = 0
			}
			return m, nil
		case "?":
			m.showHelp = true
			return m, nil
		}
	}
	return m, nil
}

// renderHelp renders the full-screen "?" keybinding reference, centered
// in the terminal. Any key closes it.
func renderHelp(width, height int) string {
	rows := [][2]string{
		{"j/k, ↑/↓", "move selection"},
		{"gg / G", "jump to first / last"},
		{"ctrl+d / ctrl+u", "half page down / up"},
		{"ctrl+n / ctrl+p", "next / previous chat"},
		{"i", "compose a message"},
		{"r", "reply to selected message"},
		{"ctrl+r", "jump to the message being replied to"},
		{"o", "open media or link"},
		{"R / x", "retry / discard a failed send"},
		{"/", "search messages in this chat"},
		{"n / p", "next / previous search match"},
		{"ctrl+/", "toggle chat list search"},
		{"esc / h", "clear filter"},
		{"q, ctrl+c", "quit"},
		{"?", "this help"},
	}
	keyStyle := lipgloss.NewStyle().Bold(true).Foreground(rcYellow)
	descStyle := lipgloss.NewStyle().Foreground(rcFg)
	keyWidth := 0
	for _, r := range rows {
		if len(r[0]) > keyWidth {
			keyWidth = len(r[0])
		}
	}
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("keybindings"))
	b.WriteString("\n\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%s  %s\n", keyStyle.Render(fmt.Sprintf("%-*s", keyWidth, r[0])), descStyle.Render(r[1]))
	}
	b.WriteString(lipgloss.NewStyle().Foreground(rcDim).Render("\npress any key to close"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(rcDim).
		Background(rcBg).
		Padding(1, 2).
		Render(b.String())
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

func (m model) View() string {
	if !m.ready {
		return "loading…"
	}
	if m.showHelp {
		return renderHelp(m.width, m.height)
	}

	listPane := renderChatList(m.visibleChats(), m.cursor, m.openJID, m.focusList, listWidth, m.height-3, m.listBottomLine())

	msgPane := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor(!m.focusList)).
		Background(rcBg).
		Width(m.msgPaneWidth()).
		Height(m.height - 3).
		Render(m.headerFor(m.openJID) + "\n" + m.titleDivider() + "\n" + m.viewport.View() + "\n" + m.sendBoxView())

	body := lipgloss.JoinHorizontal(lipgloss.Top, listPane, msgPane)

	footer := "?: help  q: quit"
	if m.composing {
		footer = "enter: send  alt+enter/ctrl+j: new line  ctrl+g: edit in $EDITOR  esc: cancel"
	} else if m.searching {
		footer = "↑/↓ ^n/^p: select  enter: done  esc/ctrl+/: cancel"
	} else if m.msgSearching {
		footer = m.matchStatus() + "^n/^p: next/prev match  enter: done  esc: cancel"
	} else if m.msgQuery != "" {
		footer = m.matchStatus() + "n/p: next/prev match  esc: clear search  ?: help"
	}
	if m.videoLoading {
		footer = lipgloss.NewStyle().Foreground(rcYellow).Render(spinnerFrames[m.videoFrame] + " downloading " + m.loadingLabel + "…")
	} else if m.hasSelectedImage() {
		if e := m.imgCache[m.openMsgs[m.selectedMsg].MsgID]; e != nil && e.err != "" {
			footer = lipgloss.NewStyle().Foreground(rcRed).Render("image preview error: " + e.err)
		}
	}
	// One row only: a wrapped footer would push the layout (and the inline
	// image's absolute position) down a line.
	footer = ansi.Truncate(footer, m.width, "…")
	if m.err != "" {
		// Drop the inline image so it isn't drawn over the error popup.
		if kittySupported() {
			footer += kittyDeleteImage(1)
		}
	} else {
		footer += m.inlineImageEscapes()
	}
	footerStyle := lipgloss.NewStyle().Foreground(rcDim).Width(m.width)

	screen := body + "\n" + footerStyle.Render(footer)
	if m.err != "" {
		screen = overlayCenter(screen, m.renderErrorPopup(), m.width, m.height)
	}
	return screen
}

// renderErrorPopup renders m.err as a red-bordered box for overlayCenter.
func (m model) renderErrorPopup() string {
	maxW := m.width * 2 / 3
	if maxW < 20 {
		maxW = m.width - 4
	}
	text := lipgloss.NewStyle().Foreground(rcFg).Width(min(lipgloss.Width(m.err), maxW)).Render(m.err)
	title := lipgloss.NewStyle().Bold(true).Foreground(rcRed).Render("error")
	hint := lipgloss.NewStyle().Foreground(rcDim).Render("press any key to close")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(rcRed).
		Background(rcBg).
		Padding(1, 2).
		Render(title + "\n\n" + text + "\n\n" + hint)
}

// overlayCenter draws fg centered on top of bg (both multi-line, possibly
// ANSI-styled), keeping bg visible around it. bg is width×height cells.
func overlayCenter(bg, fg string, width, height int) string {
	bgLines := strings.Split(bg, "\n")
	fgLines := strings.Split(fg, "\n")
	fgW := lipgloss.Width(fg)
	x := max((width-fgW)/2, 0)
	y := max((height-len(fgLines))/2, 0)
	for i, fl := range fgLines {
		row := y + i
		if row >= len(bgLines) {
			break
		}
		bl := bgLines[row]
		left := ansi.Truncate(bl, x, "")
		if pad := x - ansi.StringWidth(left); pad > 0 {
			left += strings.Repeat(" ", pad)
		}
		right := ansi.TruncateLeft(bl, x+fgW, "")
		bgLines[row] = left + "\x1b[0m" + fl + "\x1b[0m" + right
	}
	return strings.Join(bgLines, "\n")
}

// matchStatus renders the "match i/N" prefix for the in-chat search
// footer, or "no matches" when the query hits nothing.
func (m model) matchStatus() string {
	if m.msgQuery == "" {
		return ""
	}
	matches := m.messageMatches()
	if len(matches) == 0 {
		return "no matches  "
	}
	for i, idx := range matches {
		if idx == m.selectedMsg {
			return fmt.Sprintf("match %d/%d  ", i+1, len(matches))
		}
	}
	return fmt.Sprintf("%d matches  ", len(matches))
}

// sendBoxView renders the message-compose line pinned to the bottom of the
// message pane: the live textinput while composing, or a dim hint
// otherwise.
func (m model) sendBoxView() string {
	// Always 1+composeRows lines (a reply-context line, then the input or
	// hint rows) so the message pane's height never shifts between states.
	replyLine := "\n"
	switch {
	case m.msgSearching:
		m.search.Width = m.msgPaneWidth() - len(m.search.Prompt) - 1
		replyLine = m.search.View() + "\n"
	case m.replyToID != "":
		replyLine = lipgloss.NewStyle().Foreground(rcDim).Render("↩ replying to: "+m.replyPreview) + "\n"
	}
	if m.composing {
		return replyLine + m.input.View()
	}
	hint := ""
	if m.openJID != "" {
		hint = lipgloss.NewStyle().Foreground(rcDim).Render("press i to send, r to reply")
	}
	return replyLine + hint + strings.Repeat("\n", composeRows-1)
}

// listBottomLine renders the chat list's bottom row: the live filter
// textinput while searching, or a dim hint otherwise.
func (m model) listBottomLine() string {
	if m.searching {
		return m.search.View()
	}
	if m.chatFilter != "" {
		return lipgloss.NewStyle().Foreground(rcYellow).Render("/ " + m.chatFilter + " (esc to clear)")
	}
	return lipgloss.NewStyle().Foreground(rcDim).Render("ctrl+/: search chats")
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
	topRow := 4 + rel        // msgPane border(1) + header(1) + divider(1), then rel lines into the viewport
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

// titleDivider is the full-width rule drawn under the chat title, in the
// pane's border color so it reads as part of the frame.
func (m model) titleDivider() string {
	w := m.msgPaneWidth()
	if w < 1 {
		return ""
	}
	return lipgloss.NewStyle().Foreground(borderColor(!m.focusList)).Background(rcBg).
		Render(strings.Repeat("─", w))
}

func borderColor(active bool) lipgloss.Color {
	if active {
		return rcGreen
	}
	return rcBorder
}

func renderChatList(chats []Chat, cursor int, openJID string, focused bool, width, height int, bottomLine string) string {
	var b strings.Builder
	nameW := width - 10
	if nameW < 4 {
		nameW = 4
	}
	rows := height - 1 // last row is reserved for bottomLine
	start := 0
	if cursor >= rows {
		start = cursor - rows + 1
	}
	end := start + rows
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
	// The bottom line must stay one row: if it wraps, the list pane grows
	// taller than the message pane, the view overflows the terminal, and
	// bubbletea leaves stale rows (e.g. a closed search bar) on screen.
	content := b.String() + ansi.Truncate(bottomLine, width, "…")
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
func renderMessages(msgs []Message, width int, selectedIdx int, unread map[string]bool, status map[string]sendLabel) (string, []int) {
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
		bg := rcBg
		if selected {
			bg = rcMsgSelection
		}
		// Background is always set explicitly (never left to an outer
		// wrapper): lipgloss renders an empty string's width-padding as a
		// bare, unstyled reset-then-spaces, so a row with no text (the
		// blank line between messages) would otherwise show no background
		// at all across its full width.
		st := lipgloss.NewStyle().Foreground(fg).Background(bg).Width(width)
		if bold {
			st = st.Bold(true)
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
		rowBg := rcBg
		if selected {
			rowBg = rcMsgSelection
		}
		// Each segment must carry its own background: concatenating
		// separately-styled spans means each one's trailing reset (from
		// Bold/Foreground) would otherwise clobber a background set only
		// by an outer wrapper, leaving an unpainted gap behind the date.
		mark := ""
		if unread[msg.MsgID] {
			mark = lipgloss.NewStyle().Bold(true).Foreground(rcGreen).Background(rowBg).Render("● ")
		}
		name := lipgloss.NewStyle().Bold(true).Foreground(headerColor).Background(rowBg).Render(who)
		date := lipgloss.NewStyle().Foreground(rcDim).Background(rowBg).Render("  " + when)
		if st, ok := status[msg.MsgID]; ok {
			c := rcYellow
			if st.failed {
				c = rcRed
			}
			date += lipgloss.NewStyle().Foreground(c).Background(rowBg).Render("  " + st.text)
		}
		writeLine(lipgloss.NewStyle().Background(rowBg).Width(width).MaxHeight(1).Render(ansi.Truncate(mark+name+date, width, "…")))

		if autoPreviewable(msg) {
			marker := "🖼 [image]"
			switch msg.MediaType {
			case "sticker":
				marker = "🏷 [sticker]"
			case "gif":
				marker = "🎞 [gif]"
			}
			writeLine(row(marker, rcImage, true, selected))
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
		if selected && textColor == rcFg {
			textColor = rcYellow // body text stands out yellow; deleted/revoked stays red
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
