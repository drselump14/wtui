package main

import (
	"flag"
	"fmt"
	"image/color"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	rcInputBg      = lipgloss.Color("#3A3A3A") // compose box fill
	rcBorder       = lipgloss.Color("#4B4B4B") // inactive pane border
	rcGreen        = lipgloss.Color("#A5C261") // active border
	rcYellow       = lipgloss.Color("#FFC66D") // unread badges
	rcBlue         = lipgloss.Color("#6D9CBE") // "me" message header
	rcOrange       = lipgloss.Color("#CC7833") // other-sender message header
	rcRed          = lipgloss.Color("#DA4939") // errors, deleted messages
	rcImage        = lipgloss.Color("#B452CD") // messages that carry an image
	rcQuote        = lipgloss.Color("#ACA8B2") // quoted original text in a reply
	rcReact        = lipgloss.Color("#45506B") // reaction chip under a message
	rcMeBg         = lipgloss.Color("#2F3B36") // bubble tint behind my own messages, WA-style
)

type tickMsg time.Time

type chatsLoadedMsg struct {
	chats []Chat
	err   error
}

// ownMentionIDsMsg carries the signed-in account's own @mention digits,
// fetched once at startup.
type ownMentionIDsMsg struct {
	ids map[string]bool
	err error
}

// mentionsCheckedMsg reports, for each chat whose unread messages were just
// scanned for a self-mention, whether one was found (mentioned) and the
// UnreadCount that was scanned (seen), so the caller can cache it and skip
// rescanning an unchanged chat on the next refresh.
type mentionsCheckedMsg struct {
	mentioned map[string]bool
	seen      map[string]int
}

type messagesLoadedMsg struct {
	jid      string
	msgs     []Message
	names    map[string]string           // newly resolved @mention IDs → names, to merge into m.mentionNames
	reactors map[string][]reactionDetail // target MsgID → who reacted with what, to merge into m.reactors
	err      error
}

// reactionDetail is one reaction on a message: who gave it and which emoji.
// Shown both as chips under a message (renderMessages, via groupReactions)
// and, grouped the same way, by the "E" (shift+e) who-reacted popup.
type reactionDetail struct {
	emoji string
	who   string
}

// reactorTab is one emoji's worth of reactors: shown as a chip under a
// message (just its count) and as a tab in the "E" who-reacted popup (its
// full list of names).
type reactorTab struct {
	emoji string
	names []string
}

// groupReactions buckets entries by emoji, in first-seen order.
func groupReactions(entries []reactionDetail) []reactorTab {
	var tabs []reactorTab
	idx := map[string]int{}
	for _, e := range entries {
		if i, ok := idx[e.emoji]; ok {
			tabs[i].names = append(tabs[i].names, e.who)
			continue
		}
		idx[e.emoji] = len(tabs)
		tabs = append(tabs, reactorTab{emoji: e.emoji, names: []string{e.who}})
	}
	return tabs
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
	notice    string // non-error info popup (e.g. "copied message id: …"); same dismiss gesture as err, styled differently
	ready     bool
	lastKey   string // tracks "g" for the "gg" (goto top) motion
	showHelp  bool   // true while the full-screen "?" keybinding help is shown

	selectedMsg  int   // index into openMsgs the user has selected; -1 = none yet
	msgLineStart []int // msgLineStart[i] = first content line of openMsgs[i]; len = len(openMsgs)+1

	imgCache map[string]*imgEntry

	// mentionNames caches @mention IDs (raw LID/phone digits embedded in
	// message text) resolved to the mentioned contact's saved name, so
	// loadMessages only asks wacli about ones it hasn't seen yet.
	mentionNames map[string]string

	composing    bool           // true while the send box is focused and capturing input
	pending      []pendingSend  // outgoing messages not yet confirmed by a reload (all chats), in send order
	pendingSeq   int            // counter for pending placeholders' local MsgIDs
	sendTicking  bool           // true while the sending spinner's tick loop is running
	sendFrame    int            // spinner frame for in-flight placeholders
	serverMsgs   []Message      // open chat's messages as last loaded from wacli; openMsgs adds placeholders
	input        textarea.Model // multi-line send box
	replyToID    string         // MsgID being replied to via "r"; "" means a plain send
	replyPreview string         // short text shown above the send box while replying

	draftImagePath string // staged attachment (clipboard paste or ctrl+t file) awaiting a caption + enter; "" means none
	draftFileOwned bool   // true if draftImagePath is a wtui temp file (clipboard paste) to delete once sent/replaced/dropped; false for a user file (ctrl+t), left untouched on disk

	attachingFile bool // true while the ctrl+t "attach file" path prompt is showing below the send box
	filePathInput textinput.Model

	mentionOpen bool // true while the "@name" dropdown is showing below the send box
	mentionSel  int  // highlighted candidate in the mention dropdown

	reactOpen   bool   // true while the emoji-reaction picker ("e") is showing below the message pane
	reactSel    int    // highlighted emoji in the reaction picker
	reactTarget string // MsgID being reacted to; set when the picker opens

	reactSends []reactSend // reactions sent but not yet confirmed by wacli; drives the background-task section
	reactSeq   int         // counter for reactSend ids

	delConfirmOpen      bool   // true while the "d" delete-confirmation prompt is showing
	delConfirmTarget    string // MsgID being considered for deletion
	delConfirmOnlyForMe bool   // true if the target isn't ours, so only "delete for me" is offered

	delSends []int // ids of in-flight deletes, not yet confirmed by wacli; drives the background-task section
	delSeq   int   // counter for delSends ids

	reactors map[string][]reactionDetail // target MsgID → who reacted with what; also drives the chip row under a message

	showReactors   bool         // true while the "E" who-reacted popup is shown
	reactorTabs    []reactorTab // its tabs (one per emoji), set when it opens
	reactorTabSel  int          // highlighted emoji tab, moved with h/l
	reactorNameSel int          // highlighted name within the current tab, moved with j/k

	startJID string // --chat flag's JID: opened as soon as the chat list loads; "" once consumed

	chatSearchOpen bool // true while the "ctrl+/" (ctrl+_) chat-search modal is shown
	chatSearchSel  int  // selected row within the modal's filtered results
	search         textinput.Model

	msgSearching bool   // true while the open chat's "/" search bar is focused
	msgQuery     string // live query text; jumps the selection to the nearest match

	// unread holds MsgIDs of incoming messages that arrived unread: the
	// chat's last openUnread incoming messages when it was opened, plus
	// any new incoming ones that show up while it stays open. Cleared on
	// switching chats. wacli has no per-message read flag, so this is
	// derived from the chat's unread count.
	unread     map[string]bool
	openUnread int // chat's unread count when opened; consumed by the first load

	videoLoading bool   // true while "o" is downloading media to open, or "s" to save it
	loadingLabel string // what is downloading, shown next to the spinner
	videoFrame   int    // spinner frame for the downloading indicator
	saving       bool   // true if the in-flight videoLoading download is "s" (save to disk) rather than "o" (open/play), for the footer's verb

	// ownMentionIDs holds the signed-in account's own @mention digits (its
	// phone number and, separately, its LID), fetched once at startup. A
	// mention token embeds whichever of the two the sender's client used,
	// so both are checked.
	ownMentionIDs map[string]bool

	// mentionedChats marks, for the chat list badge, chats whose unread
	// messages include an @mention of ownMentionIDs. lastUnreadSeen caches
	// the UnreadCount last checked per chat, so an unchanged unread count
	// isn't re-fetched and rescanned on every refresh tick.
	mentionedChats map[string]bool
	lastUnreadSeen map[string]int

	// lastImgEsc is the kitty image escape output last sent via tea.Raw
	// (see imageEscapes), so it's only re-sent when it changes.
	lastImgEsc string
}

func initialModel() model {
	ti := textarea.New()
	ti.Placeholder = "message…  (shift+enter: send)"
	ti.CharLimit = 4096
	ti.ShowLineNumbers = false
	ti.SetHeight(composeRows)
	// The "›" prompt is drawn outside the box by sendBoxView, so the
	// textarea itself has none.
	ti.Prompt = ""
	// Line breaks come from the compose keymap's "newline" action (see
	// Update), so the textarea's own binding is disabled.
	ti.KeyMap.InsertNewline = key.NewBinding(key.WithDisabled())
	// A filled box with one column of padding on each side, so the text
	// doesn't touch its edges. Every inner style sets the fill too: each
	// one ends in an SGR reset that would otherwise drop back to the
	// terminal's default background mid-row.
	taStyles := ti.Styles()
	fill := lipgloss.NewStyle().Background(rcInputBg)
	for _, st := range []*textarea.StyleState{&taStyles.Focused, &taStyles.Blurred} {
		st.Base = fill.Padding(0, composePadding)
		st.Text = fill.Foreground(rcFg)
		st.CursorLine = fill.Foreground(rcFg) // no highlighted cursor row
		st.Placeholder = fill.Foreground(rcDim)
		st.EndOfBuffer = fill.Foreground(rcInputBg)
		st.Prompt = fill
	}
	ti.SetStyles(taStyles)

	search := textinput.New()
	search.Prompt = "/ "
	search.Placeholder = "search chats…"
	search.CharLimit = 200

	filePathInput := textinput.New()
	filePathInput.Prompt = "📎 "
	filePathInput.Placeholder = "path to file… (enter: attach, tab: complete, esc: cancel)"
	filePathInput.CharLimit = 1024

	return model{
		focusList:      false,
		ready:          false,
		selectedMsg:    -1,
		imgCache:       map[string]*imgEntry{},
		input:          ti,
		search:         search,
		filePathInput:  filePathInput,
		mentionedChats: map[string]bool{},
		lastUnreadSeen: map[string]int{},
	}
}

// visibleChats returns the chats shown in the sidebar list, currently
// always m.chats in full — the "ctrl+/" chat search (see chatSearchOpen)
// finds chats in its own modal instead of filtering this list.
func (m model) visibleChats() []Chat {
	return m.chats
}

// matchingChats returns the chats in m.chats whose name contains query
// (case-insensitively), for the chat-search modal's results list.
func matchingChats(chats []Chat, query string) []Chat {
	if query == "" {
		return chats
	}
	q := strings.ToLower(query)
	out := make([]Chat, 0, len(chats))
	for _, c := range chats {
		if strings.Contains(strings.ToLower(c.Name), q) {
			out = append(out, c)
		}
	}
	return out
}

func (m model) Init() tea.Cmd {
	return tea.Batch(loadChats(), tick(), loadOwnMentionIDs())
}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func loadChats() tea.Cmd {
	return func() tea.Msg {
		chats, err := be.FetchChats()
		return chatsLoadedMsg{chats: chats, err: err}
	}
}

// loadOwnMentionIDs fetches the signed-in account's own @mention digits
// once at startup, so the chat list can tell a mention of "me" apart from
// an ordinary unread message.
func loadOwnMentionIDs() tea.Cmd {
	return func() tea.Msg {
		ids, err := be.FetchOwnMentionIDs()
		return ownMentionIDsMsg{ids: ids, err: err}
	}
}

// checkMentions scans each of toCheck's unread messages (at most its
// UnreadCount, capped) for an @mention of one of ownIDs, run off the main
// thread like the rest of wtui's wacli calls. A chat that errors is simply
// left out of the result, so the next refresh retries it (lastUnreadSeen
// isn't advanced for it).
func checkMentions(toCheck map[string]int, ownIDs map[string]bool) tea.Cmd {
	return func() tea.Msg {
		mentioned := map[string]bool{}
		seen := map[string]int{}
		for jid, unread := range toCheck {
			limit := unread
			if limit > 100 {
				limit = 100
			}
			msgs, err := be.FetchMessages(jid, limit)
			if err != nil {
				continue
			}
			found := false
			for _, msg := range msgs {
				if msg.FromMe {
					continue
				}
				for _, id := range mentionIDs(msg.DisplayText, nil) {
					if ownIDs[id] {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			mentioned[jid] = found
			seen[jid] = unread
		}
		return mentionsCheckedMsg{mentioned: mentioned, seen: seen}
	}
}

// loadMessages fetches jid's messages and expands any @mentions in their
// text from raw LID/phone digits to the mentioned contact's saved name.
// known is the model's mentionNames cache so far; newly resolved names
// come back on the result for the caller to merge in (this runs off the
// main goroutine, so it can't write to the model directly).
func loadMessages(jid string, known map[string]string) tea.Cmd {
	return func() tea.Msg {
		msgs, err := be.FetchMessages(jid, msgLimit)
		if err != nil {
			return messagesLoadedMsg{jid: jid, err: err}
		}

		// wacli stores a reaction as a message of its own; pull those out
		// into a target-MsgID → who-reacted-with-what map (see
		// Message.ReactionToID) so they render as a badge under the message
		// they react to, the way WhatsApp itself shows them, instead of as
		// their own timeline row.
		kept := make([]Message, 0, len(msgs))
		reactors := map[string][]reactionDetail{}
		for _, mm := range msgs {
			if strings.TrimSpace(mm.ReactionToID) != "" {
				if emoji := strings.TrimSpace(mm.ReactionEmoji); emoji != "" {
					who := mm.SenderName
					switch {
					case mm.FromMe:
						who = "me"
					case who == "":
						who = mm.SenderJID
					}
					reactors[mm.ReactionToID] = append(reactors[mm.ReactionToID], reactionDetail{emoji: emoji, who: who})
				}
				continue
			}
			kept = append(kept, mm)
		}
		msgs = kept

		var need []string
		seen := map[string]bool{}
		for _, m := range msgs {
			for _, id := range mentionIDs(m.DisplayText, known) {
				if !seen[id] {
					seen[id] = true
					need = append(need, id)
				}
			}
		}
		names, _ := be.ResolveMentionNames(need) // best-effort: unresolved mentions just show their raw ID

		all := known
		if len(names) > 0 {
			all = make(map[string]string, len(known)+len(names))
			maps.Copy(all, known)
			maps.Copy(all, names)
		}
		for i := range msgs {
			msgs[i].DisplayText = expandMentions(msgs[i].DisplayText, all)
		}
		return messagesLoadedMsg{jid: jid, msgs: msgs, names: names, reactors: reactors}
	}
}

func loadImage(jid, msgID, mediaType string) tea.Cmd {
	return func() tea.Msg {
		path, err := be.DownloadMedia(jid, msgID)
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
		if err != nil && canExtractGifFrame() {
			// x/image/webp only decodes the plain (non-extended) WebP
			// bitstream; an animated sticker's VP8X container fails with
			// "invalid format". ffmpeg (via libwebp) can pull its first
			// frame out the same way it does for "gif" messages above.
			if framePath, ferr := extractVideoFrame(path); ferr == nil {
				defer os.Remove(framePath)
				if p, fw, fh, ferr := loadImagePNG(framePath); ferr == nil {
					png, w, h, err = p, fw, fh, nil
				}
			}
		}
		return imageLoadedMsg{jid: jid, msgID: msgID, png: png, w: w, h: h, err: err}
	}
}

// reactionEmojis are the "e" picker's choices: WhatsApp's own six quick
// reactions first, then a broader common-emoji set, wrapped into a grid
// (see renderReactGrid) once they no longer fit on one row.
var reactionEmojis = []string{
	"👍", "❤️", "😂", "😮", "😢", "🙏",
	"🔥", "🎉", "👏", "😍", "😁", "😡",
	"👎", "💯", "🤔", "😭",
}

type reactSentMsg struct {
	id  int
	err error
}

// reactSend is a reaction sent but not yet confirmed by wacli, tracked only
// so the background-task section can show "sending reaction…" while it's
// in flight; wtui has no retry/discard UI for a failed one (the error
// popup still covers that).
type reactSend struct {
	id    int
	emoji string
}

// sendReaction reacts to targetID (in the open chat) with emoji, looking up
// the message's original sender for wacli's group --sender requirement.
func (m model) sendReaction(id int, targetID, emoji string) tea.Cmd {
	jid := m.openJID
	var senderJID string
	for _, msg := range m.openMsgs {
		if msg.MsgID == targetID {
			senderJID = msg.SenderJID
			break
		}
	}
	return func() tea.Msg {
		return reactSentMsg{id: id, err: be.SendReact(jid, targetID, senderJID, emoji)}
	}
}

type deleteSentMsg struct {
	id  int
	err error
}

// confirmDelete issues the delete for m.delConfirmTarget (forMe: local-only,
// safe for any message; else for everyone, which only works for one of our
// own) and closes the prompt, kicking off the send spinner if it isn't
// already running.
func (m model) confirmDelete(forMe bool) (model, tea.Cmd) {
	jid, id := m.openJID, m.delConfirmTarget
	m.delConfirmOpen = false
	m.delSeq++
	seq := m.delSeq
	m.delSends = append(m.delSends, seq)
	cmd := func() tea.Msg {
		return deleteSentMsg{id: seq, err: be.DeleteMessage(jid, id, forMe)}
	}
	if !m.sendTicking {
		m.sendTicking = true
		return m, tea.Batch(cmd, sendTick())
	}
	return m, cmd
}

type chatMarkedReadMsg struct{ err error }

// markChatReadCmd marks jid as read, so opening a chat clears its unread
// badge the way it would in the WhatsApp app itself.
func markChatReadCmd(jid string) tea.Cmd {
	return func() tea.Msg {
		return chatMarkedReadMsg{err: be.MarkChatRead(jid)}
	}
}

// chatsMarkedReadMsg reports the outcome of "M" bulk-marking every muted
// chat with unread messages as read: count is how many succeeded (shown
// even alongside a partial-failure err).
type chatsMarkedReadMsg struct {
	count int
	err   error
}

// markChatsReadCmd marks each of jids as read, one at a time the same way
// a single chat is (wacli's write commands contend for one store lock, so
// there's no point firing them concurrently). It keeps going after an
// error so one bad chat doesn't block the rest, and reports however many
// it got through alongside a combined error for the ones that failed.
func markChatsReadCmd(jids []string) tea.Cmd {
	return func() tea.Msg {
		var failed []string
		marked := 0
		for _, jid := range jids {
			if err := be.MarkChatRead(jid); err != nil {
				failed = append(failed, err.Error())
				continue
			}
			marked++
		}
		if len(failed) > 0 {
			return chatsMarkedReadMsg{count: marked, err: fmt.Errorf("%d/%d muted chats failed: %s", len(failed), len(jids), strings.Join(failed, "; "))}
		}
		return chatsMarkedReadMsg{count: marked}
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

type pastedImageMsg struct {
	path string
	err  error
}

// pasteImageCmd grabs whatever image is on the system clipboard (e.g. a
// screenshot, or an image copied from a browser) and saves it to a temp
// PNG file, staged as a draft attachment until the user adds a caption
// (optional) and sends it.
func pasteImageCmd() tea.Cmd {
	return func() tea.Msg {
		path, err := clipboardImage()
		return pastedImageMsg{path: path, err: err}
	}
}

// filePicker describes a terminal file manager that can be driven as a
// one-shot "choose a file" dialog: run full-screen in place of wtui, it
// writes the chosen path to a file of our choosing before exiting.
type filePicker struct {
	bin string
	cmd func(outFile string) *exec.Cmd
}

// filePickers are tried in order; the first one found on PATH is used by
// ctrl+t. Each is a common terminal file manager that supports writing
// its selection to a file instead of just opening it.
var filePickers = []filePicker{
	{"ranger", func(out string) *exec.Cmd { return exec.Command("ranger", "--choosefile="+out) }},
	{"lf", func(out string) *exec.Cmd { return exec.Command("lf", "-selection-path", out) }},
	{"vifm", func(out string) *exec.Cmd { return exec.Command("vifm", "--choose-files", out) }},
	{"nnn", func(out string) *exec.Cmd { return exec.Command("nnn", "-p", out) }},
}

// findFilePicker returns the first available filePicker on PATH, or nil if
// none of them are installed.
func findFilePicker() *filePicker {
	for i := range filePickers {
		if _, err := exec.LookPath(filePickers[i].bin); err == nil {
			return &filePickers[i]
		}
	}
	return nil
}

type filePickedMsg struct {
	path string // "" if the user quit the picker without choosing anything
	err  error
}

// pickFileCmd takes over the terminal to run the first available file
// manager (see filePickers) as a chooser, the same way openEditorCmd takes
// it over to run $EDITOR. The chosen path is recovered from a temp file
// the picker is told to write it to.
func pickFileCmd(picker filePicker) tea.Cmd {
	f, err := os.CreateTemp("", "wtui-pick-*")
	if err != nil {
		return func() tea.Msg { return filePickedMsg{err: err} }
	}
	out := f.Name()
	f.Close()
	os.Remove(out) // some pickers (ranger) refuse to write over an existing file

	return tea.ExecProcess(picker.cmd(out), func(err error) tea.Msg {
		defer os.Remove(out)
		if err != nil {
			return filePickedMsg{err: err}
		}
		data, rerr := os.ReadFile(out)
		if rerr != nil {
			return filePickedMsg{} // quit without choosing anything; not an error
		}
		path, _, _ := strings.Cut(string(data), "\n") // one path per line; take the first
		return filePickedMsg{path: strings.TrimSpace(path)}
	})
}

// completePath extends raw's final path segment to the longest unambiguous
// match among sibling entries in its directory, for the ctrl+t manual
// path prompt's tab-completion. A single match also gets a trailing "/"
// when it's a directory, so completion can continue into it. raw is
// returned unchanged if its directory can't be read or nothing matches.
func completePath(raw string) string {
	dir, base := filepath.Split(raw)
	readDir := dir
	if readDir == "" {
		readDir = "."
	} else {
		readDir = expandUserPath(readDir)
	}
	entries, err := os.ReadDir(readDir)
	if err != nil {
		return raw
	}
	var matches []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		matches = append(matches, name)
	}
	if len(matches) == 0 {
		return raw
	}
	common := matches[0]
	for _, name := range matches[1:] {
		n := min(len(common), len(name))
		i := 0
		for i < n && common[i] == name[i] {
			i++
		}
		common = common[:i]
	}
	if common == "" || common == base {
		return raw
	}
	return dir + common
}

// expandUserPath expands a leading "~" (or "~/...") in a user-typed file
// path to the current user's home directory; any other path is returned
// unchanged.
func expandUserPath(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// composeRows is the send box's height in text rows.
const composeRows = 4

// composePadding is the blank columns inside the compose box on each side
// of the text; composePrompt is the "› " column drawn left of the box.
const (
	composePadding = 1
	composePromptW = 2
)

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
		path, err := be.DownloadMedia(jid, msgID)
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
		path, err := be.DownloadMedia(jid, msgID)
		if err != nil {
			return videoResultMsg{err: err}
		}
		return videoResultMsg{err: exec.Command("open", path).Start()}
	}
}

// mediaSavedMsg reports the outcome of "s" saving a message's media to
// ~/Downloads: path is the file it ended up at (unused on error).
type mediaSavedMsg struct {
	path string
	err  error
}

// saveSelectedMedia downloads the message's media if it isn't already
// cached locally, then copies it into ~/Downloads under filename (the
// message's own Filename if WhatsApp sent one) or, for an image/video/
// sticker with no filename, msgID plus an extension picked from
// mimeType, so it ends up somewhere the user can find it outside wacli's
// internal store.
func saveSelectedMedia(jid, msgID, filename, mimeType string) tea.Cmd {
	return func() tea.Msg {
		path, err := saveMediaToDownloads(jid, msgID, filename, mimeType)
		return mediaSavedMsg{path: path, err: err}
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
		path, err := be.DownloadMedia(jid, msgID)
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

// isStickerPath reports whether path names a WebP image, so an attached
// file sends as a sticker (`wacli send sticker`, no caption) rather than a
// plain file. WhatsApp stickers are WebP-only, so the extension alone is
// enough to tell.
func isStickerPath(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".webp")
}

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

// oldestUnreadIndex returns the index of the first (oldest) message in
// msgs, which is ordered oldest-first, that's marked unread, or -1 if none
// is.
func oldestUnreadIndex(msgs []Message, unread map[string]bool) int {
	for i, m := range msgs {
		if unread[m.MsgID] {
			return i
		}
	}
	return -1
}

// rebuildContent re-renders the open chat's messages, highlighting the
// currently selected message.
func (m *model) rebuildContent() {
	w := m.msgPaneWidth()
	m.viewport.SetWidth(w)
	content, lineStart := renderMessages(m.openMsgs, w, m.selectedMsg, m.unread, m.sendStatusLabels(), m.reactors)
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
	if start < m.viewport.YOffset() {
		m.viewport.SetYOffset(start)
	} else if end > m.viewport.YOffset()+m.viewport.Height()-1 {
		off := end - m.viewport.Height() + 1
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
	oldCursor := m.cursor
	next, cmd := m.update(msg)
	nm := next.(model)
	// Moving the chat-list cursor previews that chat in the message pane
	// (focus stays on the list and the chat isn't marked read). The first
	// chat list load previews the top chat the same way.
	_, chatsLoaded := msg.(chatsLoadedMsg)
	if (nm.focusList && nm.cursor != oldCursor) ||
		(chatsLoaded && nm.openJID == "") {
		cmd = tea.Batch(cmd, nm.previewCursor())
	}
	// The message selected in the message pane counts as read, so its
	// unread dot clears as soon as the user lands on it: after any key
	// that leaves the message pane focused (moving with j/k, tabbing into
	// it), and on a chat's first load, which auto-selects the newest
	// message. Later refreshes that auto-follow the newest message don't
	// count, so messages arriving while you watch keep their dot.
	_, isKey := msg.(tea.KeyPressMsg)
	if (isKey || firstLoad) && !nm.focusList && !nm.composing && !nm.chatSearchOpen &&
		nm.selectedMsg >= 0 && nm.selectedMsg < len(nm.openMsgs) {
		if id := nm.openMsgs[nm.selectedMsg].MsgID; nm.unread[id] {
			delete(nm.unread, id)
			nm.rebuildContent()
			cmd = tea.Batch(cmd, markChatReadCmd(nm.openJID))
		}
	}
	// Bubble Tea v2's cell-based renderer drops escape sequences embedded
	// in View's content, so the kitty inline image is written out of band
	// with tea.Raw — and only when it changes, since each emission
	// re-transmits the whole PNG.
	if esc := nm.imageEscapes(); esc != nm.lastImgEsc {
		nm.lastImgEsc = esc
		cmd = tea.Batch(cmd, tea.Raw(esc))
	}
	return nm, cmd
}

// textareaAtEnd reports whether ta's cursor sits at the very end of its
// value. Used to gate @mention autocomplete: wtui only tracks the compose
// cursor precisely enough to autocomplete when it's at the tail of the
// draft, not in the middle of earlier text.
func textareaAtEnd(ta textarea.Model) bool {
	li := ta.LineInfo()
	// bubbles' textarea always pads a (wrapped) line's last segment with one
	// synthetic trailing space, to place the cursor past the last real
	// character — so CharWidth is always content width + 1, and the cursor
	// at true end-of-content sits at CharWidth-1, never CharWidth itself.
	return ta.Line() == ta.LineCount()-1 && li.CharOffset == li.CharWidth-1
}

// acceptMention replaces the in-progress "@query" at the end of the draft
// with "@<phone> ", the token wacli embeds for a real mention.
func (m *model) acceptMention(c mentionCandidate) {
	before, _, found := strings.CutLast(m.input.Value(), "@")
	if !found {
		return
	}
	m.input.SetValue(before + "@" + c.phone + " ")
	m.input.CursorEnd()
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

// stepChat moves the chat-list cursor one row (down when next is true)
// and opens that chat in the message pane, marking it read.
func (m model) stepChat(next bool) (tea.Model, tea.Cmd) {
	vis := m.visibleChats()
	if len(vis) == 0 {
		return m, nil
	}
	if next {
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
		m.replyToID = ""
		m.replyPreview = ""
	}
	cmds = append(cmds, loadMessages(c.JID, m.mentionNames))
	return m, tea.Batch(cmds...)
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		vpW := m.msgPaneWidth()
		vpH := m.height - 7 - composeRows // header + divider + reply-context line + send box rows, plus borders/footer/task-section
		if vpH < 3 {
			vpH = 3
		}
		if !m.ready {
			m.viewport = viewport.New(viewport.WithWidth(vpW), viewport.WithHeight(vpH))
			m.ready = true
		} else {
			m.viewport.SetWidth(vpW)
			m.viewport.SetHeight(vpH)
		}
		m.input.SetWidth(max(vpW-composePromptW, 10))
		m.search.SetWidth(listWidth - len(m.search.Prompt) - 1)
		return m, m.loadSelectedImage()

	case tickMsg:
		cmds := []tea.Cmd{loadChats(), tick()}
		if m.openJID != "" {
			cmds = append(cmds, loadMessages(m.openJID, m.mentionNames))
		}
		return m, tea.Batch(cmds...)

	case chatsLoadedMsg:
		if msg.err != nil {
			m.err = logErr(msg.err)
			return m, nil
		}
		m.err = ""
		// Keep the cursor on the same chat, not the same row: a refresh
		// reorders the list (e.g. sending moves that chat to the top), and
		// an index-based cursor would leave the highlight on whichever
		// chat now occupies the old row.
		cursorJID := ""
		if vis := m.visibleChats(); m.cursor >= 0 && m.cursor < len(vis) {
			cursorJID = vis[m.cursor].JID
		}
		m.chats = msg.chats
		if cursorJID != "" {
			if i := slices.IndexFunc(m.visibleChats(), func(c Chat) bool { return c.JID == cursorJID }); i >= 0 {
				m.cursor = i
			}
		}

		// Re-scan only chats whose unread count changed since last checked,
		// and drop the mention flag for chats that are no longer unread.
		toCheck := map[string]int{}
		for _, c := range m.chats {
			if c.UnreadCount == 0 {
				delete(m.mentionedChats, c.JID)
				delete(m.lastUnreadSeen, c.JID)
				continue
			}
			if m.lastUnreadSeen[c.JID] != c.UnreadCount {
				toCheck[c.JID] = c.UnreadCount
			}
		}
		var mentionCmd tea.Cmd
		if len(toCheck) > 0 && m.ownMentionIDs != nil {
			mentionCmd = checkMentions(toCheck, m.ownMentionIDs)
		}

		if m.startJID != "" {
			for i, c := range m.visibleChats() {
				if c.JID != m.startJID {
					continue
				}
				m.startJID = ""
				m.cursor = i
				m.focusList = false
				m.switchChat(c)
				return m, tea.Batch(markChatReadCmd(c.JID), loadMessages(c.JID, m.mentionNames), mentionCmd)
			}
			// Not found yet (e.g. wacli's store hasn't synced it in); the
			// next refresh's chatsLoadedMsg retries since startJID is kept.
		}
		if n := len(m.visibleChats()); m.cursor >= n {
			m.cursor = n - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}
		return m, mentionCmd

	case ownMentionIDsMsg:
		if msg.err != nil {
			m.err = logErr(msg.err)
			return m, nil
		}
		m.ownMentionIDs = msg.ids
		return m, nil

	case mentionsCheckedMsg:
		for jid, found := range msg.mentioned {
			if found {
				m.mentionedChats[jid] = true
			} else {
				delete(m.mentionedChats, jid)
			}
		}
		maps.Copy(m.lastUnreadSeen, msg.seen)
		return m, nil

	case previewLoadMsg:
		if msg.jid == m.openJID && m.openMsgs == nil {
			return m, loadMessages(msg.jid, m.mentionNames)
		}
		return m, nil

	case messagesLoadedMsg:
		if msg.jid != m.openJID {
			return m, nil // stale response from a chat we've since left
		}
		if msg.err != nil {
			m.err = logErr(msg.err)
			return m, nil
		}
		m.err = ""
		if len(msg.names) > 0 {
			if m.mentionNames == nil {
				m.mentionNames = map[string]string{}
			}
			maps.Copy(m.mentionNames, msg.names)
		}
		// Follow the newest message across refreshes only if the user
		// hadn't scrolled back to read older ones.
		firstLoad := m.selectedMsg < 0
		wasAtNewest := firstLoad || m.selectedMsg >= len(m.openMsgs)-1
		m.markUnread(msg.msgs)
		if msg.msgs == nil {
			msg.msgs = []Message{}
		}
		m.serverMsgs = msg.msgs
		m.reactors = msg.reactors
		m.dropDoneSends(msg.jid, msg.msgs)
		m.mergePending()
		landedOnUnread := false
		switch {
		case firstLoad:
			// Cursor lands on the oldest unread message, the way WhatsApp
			// itself opens a chat, so unread text isn't skipped past; with
			// nothing unread, fall back to the newest message as before.
			if idx := oldestUnreadIndex(m.openMsgs, m.unread); idx >= 0 {
				m.selectedMsg = idx
				landedOnUnread = true
			} else {
				m.selectedMsg = len(m.openMsgs) - 1
			}
		case wasAtNewest || m.selectedMsg >= len(m.openMsgs):
			m.selectedMsg = len(m.openMsgs) - 1
		}
		m.rebuildContent()
		if landedOnUnread {
			// Scroll to the newest message so the chat still opens showing
			// what's current; the cursor stays on the oldest unread message
			// until the user moves it, at which point scrollToSelection
			// takes over as normal.
			m.viewport.GotoBottom()
		} else {
			m.scrollToSelection()
		}
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
			e.err = logErr(msg.err)
		} else {
			e.png = msg.png
			e.w, e.h = msg.w, msg.h
		}
		return m, nil

	case chatMarkedReadMsg:
		if msg.err != nil {
			m.err = logErr(msg.err)
		}
		return m, nil

	case chatsMarkedReadMsg:
		if msg.err != nil {
			m.err = logErr(msg.err)
		} else {
			m.notice = "marked " + pluralize(msg.count, "muted chat") + " as read"
		}
		return m, nil

	case reactSentMsg:
		for i, r := range m.reactSends {
			if r.id == msg.id {
				m.reactSends = append(m.reactSends[:i], m.reactSends[i+1:]...)
				break
			}
		}
		if msg.err != nil {
			m.err = logErr(msg.err)
			return m, nil
		}
		return m, loadMessages(m.openJID, m.mentionNames)

	case deleteSentMsg:
		for i, id := range m.delSends {
			if id == msg.id {
				m.delSends = append(m.delSends[:i], m.delSends[i+1:]...)
				break
			}
		}
		if msg.err != nil {
			m.err = logErr(msg.err)
			return m, nil
		}
		return m, loadMessages(m.openJID, m.mentionNames)

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
			m.err = logErr(msg.err)
			return m, m.input.Focus()
		}
		m.err = ""
		m.input.SetValue(msg.text)
		m.input.CursorEnd()
		return m, m.input.Focus()

	case pastedImageMsg:
		if msg.err != nil {
			m.err = logErr(msg.err)
			return m, m.input.Focus()
		}
		m.err = ""
		if m.draftImagePath != "" && m.draftFileOwned {
			os.Remove(m.draftImagePath) // replacing an earlier staged paste
		}
		m.draftImagePath = msg.path
		m.draftFileOwned = true
		return m, m.input.Focus()

	case filePickedMsg:
		if msg.err != nil {
			m.err = logErr(msg.err)
			return m, m.input.Focus()
		}
		if msg.path == "" {
			return m, m.input.Focus() // quit the picker without choosing anything
		}
		path := expandUserPath(msg.path)
		if info, serr := os.Stat(path); serr != nil {
			m.err = logErr(serr)
			return m, m.input.Focus()
		} else if info.IsDir() {
			m.err = "can't attach a directory"
			return m, m.input.Focus()
		}
		if m.draftImagePath != "" && m.draftFileOwned {
			os.Remove(m.draftImagePath) // replacing an earlier staged attachment
		}
		m.draftImagePath = path
		m.draftFileOwned = false // references the user's own file; never deleted by wtui
		return m, m.input.Focus()

	case videoResultMsg:
		m.videoLoading, m.saving = false, false
		if msg.err != nil {
			m.err = logErr(msg.err)
		}
		return m, nil

	case mediaSavedMsg:
		m.videoLoading, m.saving = false, false
		if msg.err != nil {
			m.err = logErr(msg.err)
		} else {
			m.notice = "saved to " + msg.path
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
			m.err = logErr(msg.err)
		}
		return m, nil

	case tea.PasteMsg:
		// Bubble Tea v2 delivers bracketed paste as its own message rather
		// than a key, so route it to whichever text input has focus, with
		// the same follow-up each input's typing branch does.
		var cmd tea.Cmd
		switch {
		case m.err != "" || m.notice != "" || m.showHelp || m.showReactors:
		case m.composing && m.attachingFile:
			m.filePathInput, cmd = m.filePathInput.Update(msg)
		case m.composing:
			m.input, cmd = m.input.Update(msg)
			if query, ok := activeMentionQuery(m.input.Value()); ok && textareaAtEnd(m.input) {
				m.mentionOpen = len(mentionCandidatesFor(m.openMsgs, query)) > 0
				m.mentionSel = 0
			} else {
				m.mentionOpen = false
			}
		case m.chatSearchOpen:
			m.search, cmd = m.search.Update(msg)
			m.chatSearchSel = 0
		case m.msgSearching:
			m.search, cmd = m.search.Update(msg)
			m.msgQuery = m.search.Value()
			m.jumpToMessageMatch(max(m.selectedMsg, 0), m.msgQuery)
		}
		return m, cmd

	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" || appKeys.action("global", key) == "quit" {
			return m, tea.Quit
		}

		// An error or notice popup swallows the next key to dismiss it.
		if m.err != "" || m.notice != "" {
			m.err = ""
			m.notice = ""
			return m, nil
		}
		if m.showHelp {
			m.showHelp = false
			return m, nil
		}
		if m.showReactors {
			switch appKeys.action("reactors", key) {
			case "prevTab":
				if n := len(m.reactorTabs); n > 0 {
					m.reactorTabSel = (m.reactorTabSel - 1 + n) % n
					m.reactorNameSel = 0
				}
			case "nextTab":
				if n := len(m.reactorTabs); n > 0 {
					m.reactorTabSel = (m.reactorTabSel + 1) % n
					m.reactorNameSel = 0
				}
			case "prevName":
				if m.reactorNameSel > 0 {
					m.reactorNameSel--
				}
			case "nextName":
				if m.reactorTabSel < len(m.reactorTabs) {
					if n := len(m.reactorTabs[m.reactorTabSel].names); m.reactorNameSel < n-1 {
						m.reactorNameSel++
					}
				}
			default:
				m.showReactors = false
			}
			return m, nil
		}
		if m.composing {
			if m.attachingFile {
				switch appKeys.action("attach", key) {
				case "cancel":
					m.attachingFile = false
					m.filePathInput.Reset()
					m.filePathInput.Blur()
					return m, nil
				case "confirm":
					raw := strings.TrimSpace(m.filePathInput.Value())
					if raw == "" {
						return m, nil
					}
					path := expandUserPath(raw)
					info, err := os.Stat(path)
					if err != nil {
						m.err = err.Error()
						return m, nil
					}
					if info.IsDir() {
						m.err = "can't attach a directory"
						return m, nil
					}
					if m.draftImagePath != "" && m.draftFileOwned {
						os.Remove(m.draftImagePath) // replacing an earlier staged attachment
					}
					m.draftImagePath = path
					m.draftFileOwned = false // references the user's own file; never deleted by wtui
					m.attachingFile = false
					m.filePathInput.Reset()
					m.filePathInput.Blur()
					return m, nil
				case "complete":
					m.filePathInput.SetValue(completePath(m.filePathInput.Value()))
					m.filePathInput.CursorEnd()
					return m, nil
				default:
					var cmd tea.Cmd
					m.filePathInput, cmd = m.filePathInput.Update(msg)
					return m, cmd
				}
			}
			switch appKeys.action("compose", key) {
			case "cancel":
				if m.mentionOpen {
					// First esc just dismisses the dropdown, leaving the
					// draft (including the "@" typed so far) untouched.
					m.mentionOpen = false
					return m, nil
				}
				if m.draftImagePath != "" {
					// First esc drops the staged attachment and leaves
					// the draft text and reply context alone; a second
					// esc then cancels composing as usual.
					if m.draftFileOwned {
						os.Remove(m.draftImagePath)
					}
					m.draftImagePath = ""
					m.draftFileOwned = false
					return m, nil
				}
				m.composing = false
				m.replyToID = ""
				m.replyPreview = ""
				m.input.Blur()
				m.input.Reset()
				return m, nil
			case "vertical":
				if m.mentionOpen {
					query, _ := activeMentionQuery(m.input.Value())
					if n := len(mentionCandidatesFor(m.openMsgs, query)); n > 0 {
						if key == "up" {
							m.mentionSel = (m.mentionSel - 1 + n) % n
						} else {
							m.mentionSel = (m.mentionSel + 1) % n
						}
					}
					return m, nil
				}
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				return m, cmd
			case "switchChat":
				if m.mentionOpen {
					return m, nil // switching chats mid-mention would strand the dropdown on the old chat's senders
				}
				// Switch chats without leaving the send box. The draft
				// text and any staged image carry over; the reply
				// context belongs to the old chat, so it is dropped.
				return m.stepChat(key == "ctrl+n")
			case "openEditor":
				return m, openEditorCmd(m.input.Value())
			case "pasteImage":
				if m.openJID == "" {
					return m, nil
				}
				return m, pasteImageCmd()
			case "attachFile":
				if m.openJID == "" {
					return m, nil
				}
				if picker := findFilePicker(); picker != nil {
					return m, pickFileCmd(*picker)
				}
				// No ranger/lf/vifm/nnn on PATH: fall back to typing the
				// path by hand, with tab-completion.
				m.attachingFile = true
				m.filePathInput.Reset()
				return m, m.filePathInput.Focus()
			case "newline", "acceptMention":
				// With the @mention dropdown open, enter/tab pick the
				// highlighted candidate; otherwise enter inserts a line
				// break and tab does nothing.
				if m.mentionOpen {
					if query, ok := activeMentionQuery(m.input.Value()); ok {
						if cands := mentionCandidatesFor(m.openMsgs, query); len(cands) > 0 {
							m.acceptMention(cands[m.mentionSel%len(cands)])
							m.mentionOpen = false
							m.mentionSel = 0
							return m, nil
						}
					}
					m.mentionOpen = false
				}
				if appKeys.action("compose", key) == "acceptMention" {
					return m, nil
				}
				// InsertRune rather than forwarding the key, so a remapped
				// "newline" key works without the textarea knowing it.
				m.input.InsertRune('\n')
				return m, nil
			case "send":
				if m.mentionOpen {
					if query, ok := activeMentionQuery(m.input.Value()); ok {
						if cands := mentionCandidatesFor(m.openMsgs, query); len(cands) > 0 {
							m.acceptMention(cands[m.mentionSel%len(cands)])
							m.mentionOpen = false
							m.mentionSel = 0
							return m, nil
						}
					}
					m.mentionOpen = false
				}
				text := strings.TrimSpace(m.input.Value())
				if m.openJID == "" || (text == "" && m.draftImagePath == "") {
					return m, nil
				}
				replyTo := m.replyToID
				path, owned := m.draftImagePath, m.draftFileOwned
				mentions := pendingMentionPhones(text)
				m.input.Reset()
				m.replyToID = ""
				m.replyPreview = ""
				m.draftImagePath = ""
				m.draftFileOwned = false
				// Stay in compose mode so the next message can be typed
				// right away while this one sends in the background.
				if path != "" {
					if isStickerPath(path) {
						// Stickers can't carry a caption, unlike any other
						// attached file; whatever text was typed alongside
						// it is simply dropped.
						return m, m.queueStickerSend(m.openJID, path, replyTo, owned)
					}
					return m, m.queueMediaSend(m.openJID, path, text, replyTo, owned)
				}
				return m, m.queueSend(m.openJID, text, replyTo, mentions)
			default:
				var cmd tea.Cmd
				m.input, cmd = m.input.Update(msg)
				if query, ok := activeMentionQuery(m.input.Value()); ok && textareaAtEnd(m.input) {
					m.mentionOpen = len(mentionCandidatesFor(m.openMsgs, query)) > 0
					m.mentionSel = 0
				} else {
					m.mentionOpen = false
				}
				return m, cmd
			}
		}

		if m.chatSearchOpen {
			switch appKeys.action("search", key) {
			case "close":
				// ctrl+/ toggles the chat search modal: pressing it again
				// while it's open closes it, same as esc. The sidebar was
				// never touched, so there's nothing to undo.
				m.chatSearchOpen = false
				m.search.Blur()
				m.search.Reset()
				return m, nil
			case "commit":
				// Jump the real chat list to the highlighted result and
				// open it, same as pressing enter on it directly.
				matches := matchingChats(m.chats, m.search.Value())
				m.chatSearchOpen = false
				m.search.Blur()
				if m.chatSearchSel < 0 || m.chatSearchSel >= len(matches) {
					return m, nil
				}
				c := matches[m.chatSearchSel]
				for i, cc := range m.chats {
					if cc.JID == c.JID {
						m.cursor = i
						break
					}
				}
				m.focusList = false
				cmds := []tea.Cmd{markChatReadCmd(c.JID)}
				if c.JID != m.openJID {
					m.switchChat(c)
				}
				if m.openMsgs == nil {
					cmds = append(cmds, loadMessages(c.JID, m.mentionNames))
				}
				return m, tea.Batch(cmds...)
			case "prev":
				if m.chatSearchSel > 0 {
					m.chatSearchSel--
				}
				return m, nil
			case "next":
				if n := len(matchingChats(m.chats, m.search.Value())); m.chatSearchSel < n-1 {
					m.chatSearchSel++
				}
				return m, nil
			default:
				old := m.search.Value()
				var cmd tea.Cmd
				m.search, cmd = m.search.Update(msg)
				if m.search.Value() != old {
					m.chatSearchSel = 0
				}
				return m, cmd
			}
		}

		if m.msgSearching {
			switch appKeys.action("msgsearch", key) {
			case "close":
				m.msgSearching = false
				m.msgQuery = ""
				m.search.Blur()
				m.search.Reset()
				return m, nil
			case "commit":
				m.msgSearching = false
				m.search.Blur()
				if len(m.messageMatches()) == 0 {
					m.msgQuery = ""
				}
				return m, m.loadSelectedImage()
			case "next":
				m.nextMessageMatch(1)
				return m, nil
			case "prev":
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

		if m.delConfirmOpen {
			switch appKeys.action("delconfirm", key) {
			case "cancel":
				m.delConfirmOpen = false
				return m, nil
			case "forMe":
				return m.confirmDelete(true)
			case "everyone":
				if m.delConfirmOnlyForMe {
					return m, nil // "for everyone" wasn't offered; ignore
				}
				return m.confirmDelete(false)
			}
			return m, nil
		}

		if m.reactOpen {
			switch appKeys.action("react", key) {
			case "cancel":
				m.reactOpen = false
				return m, nil
			case "prev":
				m.reactSel = (m.reactSel - 1 + len(reactionEmojis)) % len(reactionEmojis)
				return m, nil
			case "next":
				m.reactSel = (m.reactSel + 1) % len(reactionEmojis)
				return m, nil
			case "up":
				if cols := reactGridCols(m.msgPaneWidth()); m.reactSel-cols >= 0 {
					m.reactSel -= cols
				}
				return m, nil
			case "down":
				if cols := reactGridCols(m.msgPaneWidth()); m.reactSel+cols < len(reactionEmojis) {
					m.reactSel += cols
				}
				return m, nil
			case "confirm":
				m.reactOpen = false
				emoji := reactionEmojis[m.reactSel]
				m.reactSeq++
				id := m.reactSeq
				m.reactSends = append(m.reactSends, reactSend{id: id, emoji: emoji})
				cmd := m.sendReaction(id, m.reactTarget, emoji)
				if !m.sendTicking {
					m.sendTicking = true
					cmd = tea.Batch(cmd, sendTick())
				}
				return m, cmd
			}
			return m, nil
		}

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

		switch appKeys.action("browse", key) {
		case "quit":
			return m, tea.Quit
		case "bottom":
			if m.focusList {
				m.cursor = len(m.visibleChats()) - 1
			} else if len(m.openMsgs) > 0 {
				m.selectedMsg = len(m.openMsgs) - 1
				m.rebuildContent()
				m.scrollToSelection()
			}
			return m, m.loadSelectedImage()
		case "halfPageDown":
			if m.focusList {
				m.cursor += (m.height - 4) / 2
				if n := len(m.visibleChats()); m.cursor > n-1 {
					m.cursor = n - 1
				}
			} else {
				m.viewport.HalfPageDown()
			}
			return m, nil
		case "halfPageUp":
			if m.focusList {
				m.cursor -= (m.height - 4) / 2
				if m.cursor < 0 {
					m.cursor = 0
				}
			} else {
				m.viewport.HalfPageUp()
			}
			return m, nil
		case "up":
			// The arrow key also drives the chat list when it has focus;
			// "vimUp" (k, below) deliberately doesn't — the list is
			// navigated with ctrl+n/ctrl+p instead.
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
		case "vimUp":
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
		case "vimDown":
			if m.focusList {
				return m, nil
			}
			if m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs)-1 {
				m.selectedMsg++
				m.rebuildContent()
				m.scrollToSelection()
			}
			return m, m.loadSelectedImage()
		case "pageUp":
			m.viewport.PageUp()
			return m, nil
		case "pageDown":
			m.viewport.PageDown()
			return m, nil
		case "searchMessages":
			if m.openJID == "" || len(m.openMsgs) == 0 {
				return m, nil
			}
			m.msgSearching = true
			m.msgQuery = ""
			m.search.Prompt = "/ "
			m.search.Placeholder = "search messages…"
			m.search.Reset()
			return m, m.search.Focus()
		case "searchChats":
			m.chatSearchOpen = true
			m.chatSearchSel = 0
			m.search.Prompt = "/ "
			m.search.Placeholder = "search chats…"
			m.search.Reset()
			return m, m.search.Focus()
		case "open":
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
						cmds = append(cmds, loadMessages(c.JID, m.mentionNames))
					}
					return m, tea.Batch(cmds...)
				}
			}
			return m, nil
		case "openMedia":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				return m.openSelected()
			}
			return m, nil
		case "saveMedia":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				sel := m.openMsgs[m.selectedMsg]
				if isPendingID(sel.MsgID) {
					m.err = "can't save: message hasn't finished sending yet"
					return m, nil
				}
				if sel.MediaType == "" || sel.Revoked || sel.DeletedForMe {
					m.err = "nothing to save: message has no media"
					return m, nil
				}
				if m.videoLoading {
					return m, nil
				}
				m.videoLoading, m.saving, m.err = true, true, ""
				m.loadingLabel = sel.MediaType
				return m, tea.Batch(saveSelectedMedia(m.openJID, sel.MsgID, sel.Filename, sel.MimeType), videoTick())
			}
			return m, nil
		case "compose":
			if !m.focusList && m.openJID != "" {
				m.composing = true
				return m, m.input.Focus()
			}
			return m, nil
		case "markChatRead":
			if m.openJID == "" {
				return m, nil
			}
			for id := range m.unread {
				delete(m.unread, id)
			}
			m.rebuildContent()
			return m, markChatReadCmd(m.openJID)
		case "markMutedRead":
			var toMark []string
			for _, c := range m.chats {
				if c.Muted() && c.UnreadCount > 0 {
					toMark = append(toMark, c.JID)
				}
			}
			if len(toMark) == 0 {
				m.notice = "no muted chats have unread messages"
				return m, nil
			}
			if m.openJID != "" && slices.Contains(toMark, m.openJID) {
				for id := range m.unread {
					delete(m.unread, id)
				}
				m.rebuildContent()
			}
			return m, markChatsReadCmd(toMark)
		case "reply":
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
		case "react":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				sel := m.openMsgs[m.selectedMsg]
				if isPendingID(sel.MsgID) {
					m.err = "can't react to a message that hasn't been sent yet"
					return m, nil
				}
				m.reactOpen = true
				m.reactSel = 0
				m.reactTarget = sel.MsgID
			}
			return m, nil
		case "delete":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				sel := m.openMsgs[m.selectedMsg]
				if isPendingID(sel.MsgID) {
					m.err = "can't delete a message that hasn't been sent yet"
					return m, nil
				}
				if sel.Revoked || sel.DeletedForMe {
					m.err = "message already deleted"
					return m, nil
				}
				m.delConfirmOpen = true
				m.delConfirmTarget = sel.MsgID
				m.delConfirmOnlyForMe = !sel.FromMe
			}
			return m, nil
		case "copyText":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				sel := m.openMsgs[m.selectedMsg]
				text := captionAwareText(sel)
				if text == "" {
					m.err = "nothing to copy: message has no text"
				} else if err := copyToClipboard(text); err != nil {
					m.err = logErr(fmt.Errorf("copy message text: %w", err))
				} else {
					m.notice = "copied message text"
				}
			}
			return m, nil
		case "copyID":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				id := m.openMsgs[m.selectedMsg].MsgID
				if err := copyToClipboard(id); err != nil {
					m.err = logErr(fmt.Errorf("copy message id: %w", err))
				} else {
					m.notice = "copied message id: " + id
				}
			}
			return m, nil
		case "showReactors":
			if !m.focusList && m.selectedMsg >= 0 && m.selectedMsg < len(m.openMsgs) {
				if entries := m.reactors[m.openMsgs[m.selectedMsg].MsgID]; len(entries) > 0 {
					m.reactorTabs = groupReactions(entries)
					m.reactorTabSel = 0
					m.reactorNameSel = 0
					m.showReactors = true
				} else {
					m.err = "no reactions on this message"
				}
			}
			return m, nil
		case "retrySend":
			if m.focusList || m.selectedMsg < 0 || m.selectedMsg >= len(m.openMsgs) {
				return m, nil
			}
			id := m.openMsgs[m.selectedMsg].MsgID
			cmd := m.retrySend(id)
			m.mergePending()
			m.rebuildContent()
			if cmd != nil && !m.sendTicking {
				m.sendTicking = true
				cmd = tea.Batch(cmd, sendTick())
			}
			return m, cmd
		case "dismissFailedSend":
			if m.focusList || m.selectedMsg < 0 || m.selectedMsg >= len(m.openMsgs) {
				return m, nil
			}
			id := m.openMsgs[m.selectedMsg].MsgID
			if !m.dropFailedSend(id) {
				return m, nil
			}
			m.mergePending()
			m.rebuildContent()
			return m, nil
		case "jumpToQuoted":
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
		case "switchChat":
			return m.stepChat(key == "ctrl+n")
		case "nextMatch":
			if m.msgQuery == "" {
				return m, nil
			}
			m.nextMessageMatch(1)
			return m, m.loadSelectedImage()
		case "prevMatch":
			if m.msgQuery == "" {
				return m, nil
			}
			m.nextMessageMatch(-1)
			return m, m.loadSelectedImage()
		case "clearFilter":
			if m.msgQuery != "" {
				m.msgQuery = ""
				m.search.Reset()
			}
			return m, nil
		case "help":
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
		{"ctrl+n / ctrl+p", "next / previous chat (also while composing)"},
		{"i", "compose a message (shift+enter sends, enter adds a new line)"},
		{"ctrl+v", "paste a clipboard image (while composing)"},
		{"ctrl+t", "attach a file from this pc: ranger/lf/vifm/nnn if found, else type a path (while composing); a .webp sends as a sticker"},
		{"@", "mention a sender in this chat (tab/enter picks, ↑/↓ moves)"},
		{"A", "mark all of this chat's messages as read"},
		{"M", "mark all muted chats' unread messages as read"},
		{"r", "reply to selected message"},
		{"e", "react to selected message (←/→/↑/↓ pick, enter send)"},
		{"E", "who reacted to selected message (h/l emoji tabs, j/k names)"},
		{"f", "jump to the message being replied to"},
		{"o", "open media or link"},
		{"s", "save selected message's media to ~/Downloads"},
		{"R / x", "retry / discard a failed send"},
		{"d", "delete selected message (for me, or for everyone if it's yours)"},
		{"y", "copy selected message's text to clipboard"},
		{"Y", "copy selected message's id to clipboard (debugging)"},
		{"/", "search messages in this chat"},
		{"n / p", "next / previous search match"},
		{keyHint("browse", "searchChats", "^g"), "search chats (opens a modal)"},
		{"esc / h", "clear in-chat message search"},
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

// renderChatSearch draws the "ctrl+/" chat-search modal: a query box over a
// scrollable list of matching chats, centered like renderHelp. It never
// touches the real sidebar — picking a result (see "commit" in Update)
// jumps the real cursor to it and opens it in one step.
func renderChatSearch(matches []Chat, sel int, searchView string, boxW, width, height int) string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("search chats"))
	b.WriteString("\n\n")
	b.WriteString(searchView)
	b.WriteString("\n\n")

	rows := max(height-14, 3)
	if len(matches) == 0 {
		b.WriteString(lipgloss.NewStyle().Foreground(rcDim).Render("no matching chats"))
		b.WriteString("\n")
	} else {
		if rows > len(matches) {
			rows = len(matches)
		}
		start := 0
		if sel >= rows {
			start = sel - rows + 1
		}
		end := min(start+rows, len(matches))
		nameW := boxW - 10
		if nameW < 4 {
			nameW = 4
		}
		for i := start; i < end; i++ {
			c := matches[i]
			// Row format and unread/muted coloring match the sidebar's
			// renderChatList exactly, minus its mention (@) badge, which
			// has no equivalent here.
			icon := "👤"
			if c.Kind == "group" {
				icon = "👥"
			} else if c.Kind == "newsletter" || c.Kind == "channel" {
				icon = "📢"
			}
			badge := ""
			if c.UnreadCount > 0 {
				badge = fmt.Sprintf(" (%d)", c.UnreadCount)
			}
			line := fmt.Sprintf("%s %-*s%s", icon, nameW, truncate(c.Name, nameW), badge)
			style := lipgloss.NewStyle().Foreground(rcFg)
			switch {
			case c.UnreadCount > 0 && c.Muted():
				style = style.Foreground(rcDim)
			case c.UnreadCount > 0:
				style = style.Foreground(rcYellow)
			}
			if i == sel {
				style = style.Background(rcSelection)
			}
			b.WriteString(style.Render(line))
			b.WriteString("\n")
		}
	}
	b.WriteString(lipgloss.NewStyle().Foreground(rcDim).Render("\n↑/↓ ^n/^p: select · enter: open · esc: close"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(rcDim).
		Background(rcBg).
		Padding(1, 2).
		Width(boxW + 2). // lipgloss v2 counts the border in Width
		Render(b.String())
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

// renderReactors draws the "E" who-reacted popup: one "emoji  name" line per
// reaction on the selected message, in a centered box like renderHelp.
func renderReactors(tabs []reactorTab, tabSel, nameSel, width, height int) string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("reactions"))
	b.WriteString("\n\n")

	chips := make([]string, len(tabs))
	for i, t := range tabs {
		label := fmt.Sprintf(" %s %d ", t.emoji, len(t.names))
		if i == tabSel {
			chips[i] = lipgloss.NewStyle().Bold(true).Background(rcYellow).Foreground(rcBg).Render(label)
		} else {
			chips[i] = lipgloss.NewStyle().Foreground(rcFg).Render(label)
		}
	}
	b.WriteString(strings.Join(chips, " "))
	b.WriteString("\n\n")

	if tabSel >= 0 && tabSel < len(tabs) {
		for i, who := range tabs[tabSel].names {
			style := lipgloss.NewStyle().Foreground(rcFg)
			if i == nameSel {
				style = lipgloss.NewStyle().Background(rcMsgSelection).Foreground(rcYellow)
			}
			b.WriteString(style.Render(who))
			b.WriteString("\n")
		}
	}
	b.WriteString(lipgloss.NewStyle().Foreground(rcDim).Render("\nh/l switch emoji · j/k move · any other key closes"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(rcDim).
		Background(rcBg).
		Padding(1, 2).
		Render(b.String())
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

// View wraps viewContent in Bubble Tea v2's declarative tea.View:
// fullscreen (alt screen) for the app's whole lifetime.
func (m model) View() tea.View {
	v := tea.NewView(m.viewContent())
	v.AltScreen = true
	return v
}

func (m model) viewContent() string {
	if !m.ready {
		return "loading…"
	}
	if m.showReactors {
		return renderReactors(m.reactorTabs, m.reactorTabSel, m.reactorNameSel, m.width, m.height)
	}
	if m.showHelp {
		return renderHelp(m.width, m.height)
	}
	if m.chatSearchOpen {
		boxW := min(m.width-4, 50)
		m.search.SetWidth(boxW - len(m.search.Prompt) - 1)
		matches := matchingChats(m.chats, m.search.Value())
		if m.chatSearchSel >= len(matches) {
			m.chatSearchSel = max(len(matches)-1, 0)
		}
		return renderChatSearch(matches, m.chatSearchSel, m.search.View(), boxW, m.width, m.height)
	}

	listPane := renderChatList(m.visibleChats(), m.cursor, m.openJID, m.focusList, listWidth, m.height-4, m.listBottomLine(), m.mentionedChats)

	msgPane := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor(!m.focusList)).
		Background(rcBg).
		// lipgloss v2 counts the border in Width/Height, hence the +2s.
		Width(m.msgPaneWidth() + 2).
		Height(m.height - 4 + 2).
		Render(m.headerFor(m.openJID) + "\n" + m.titleDivider() + "\n" + m.viewport.View() + "\n" + m.sendBoxView())

	body := lipgloss.JoinHorizontal(lipgloss.Top, listPane, msgPane)

	// Background-task section: one row, always reserved so the layout
	// never shifts when a send/reaction starts or finishes; blank when
	// nothing is in flight.
	taskLine := ansi.Truncate(m.bgTaskSummary(), m.width, "…")
	taskLine = lipgloss.NewStyle().Foreground(rcYellow).Width(m.width).Render(taskLine)

	footer := "?: help  q: quit"
	if m.composing {
		footer = "shift+enter: send  enter: new line  ctrl+g: edit in $EDITOR  ctrl+v: paste image  esc: cancel"
	} else if m.msgSearching {
		footer = m.matchStatus() + "^n/^p: next/prev match  enter: done  esc: cancel"
	} else if m.msgQuery != "" {
		footer = m.matchStatus() + "n/p: next/prev match  esc: clear search  ?: help"
	}
	if m.videoLoading {
		verb := "downloading"
		if m.saving {
			verb = "saving"
		}
		footer = lipgloss.NewStyle().Foreground(rcYellow).Render(spinnerFrames[m.videoFrame] + " " + verb + " " + m.loadingLabel + "…")
	} else if m.hasSelectedImage() {
		if e := m.imgCache[m.openMsgs[m.selectedMsg].MsgID]; e != nil && e.err != "" {
			footer = lipgloss.NewStyle().Foreground(rcRed).Render("image preview error: " + e.err)
		}
	}
	// One row only: a wrapped footer would push the layout (and the inline
	// image's absolute position) down a line.
	footer = ansi.Truncate(footer, m.width, "…")
	footerStyle := lipgloss.NewStyle().Foreground(rcDim).Width(m.width)

	screen := body + "\n" + taskLine + "\n" + footerStyle.Render(footer)
	if m.err != "" {
		screen = overlayCenter(screen, m.renderPopup(m.err, "error", rcRed), m.width, m.height)
	} else if m.notice != "" {
		screen = overlayCenter(screen, m.renderPopup(m.notice, "notice", rcGreen), m.width, m.height)
	}
	return screen
}

// renderPopup renders msg as a bordered box for overlayCenter, titled and
// colored by accent (red for m.err, green for m.notice).
func (m model) renderPopup(msg, title string, accent color.Color) string {
	maxW := m.width * 2 / 3
	if maxW < 20 {
		maxW = m.width - 4
	}
	text := lipgloss.NewStyle().Foreground(rcFg).Width(min(lipgloss.Width(msg), maxW)).Render(msg)
	titleStyled := lipgloss.NewStyle().Bold(true).Foreground(accent).Render(title)
	hint := lipgloss.NewStyle().Foreground(rcDim).Render("press any key to close")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Background(rcBg).
		Padding(1, 2).
		Render(titleStyled + "\n\n" + text + "\n\n" + hint)
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
	if m.delConfirmOpen {
		return renderDeleteConfirm(m.delConfirmOnlyForMe)
	}
	if m.reactOpen {
		// Its own fixed-height block (see renderReactGrid) rather than a
		// single replyLine row: the emoji grid can wrap to several lines.
		return renderReactGrid(m.reactSel, m.msgPaneWidth())
	}
	// Always 1+composeRows lines (a reply-context line, then the input or
	// hint rows) so the message pane's height never shifts between states.
	replyLine := "\n"
	switch {
	case m.composing && m.attachingFile:
		m.filePathInput.SetWidth(m.msgPaneWidth() - len(m.filePathInput.Prompt) - 1)
		replyLine = m.filePathInput.View() + "\n"
	case m.composing && m.mentionOpen:
		query, _ := activeMentionQuery(m.input.Value())
		replyLine = renderMentionRow(mentionCandidatesFor(m.openMsgs, query), m.mentionSel) + "\n"
	case m.msgSearching:
		m.search.SetWidth(m.msgPaneWidth() - len(m.search.Prompt) - 1)
		replyLine = m.search.View() + "\n"
	case m.replyToID != "" || m.draftImagePath != "":
		var parts []string
		if m.replyToID != "" {
			label := lipgloss.NewStyle().Foreground(rcYellow).Render("↩ replying to:")
			preview := lipgloss.NewStyle().Foreground(rcFg).Render(m.replyPreview)
			parts = append(parts, label+" "+preview)
		}
		if m.draftImagePath != "" {
			verb := "attached"
			if isStickerPath(m.draftImagePath) {
				verb = "staged as sticker, no caption"
			}
			attached := lipgloss.NewStyle().Foreground(rcFg).
				Render(fmt.Sprintf("📎 %s %s (esc to remove)", filepath.Base(m.draftImagePath), verb))
			parts = append(parts, attached)
		}
		replyLine = strings.Join(parts, "   ") + "\n"
	}
	if m.composing {
		// "› " beside the box's first row, blank beside the rest, both on
		// the pane background so only the box itself is filled.
		prompt := lipgloss.NewStyle().Background(rcBg).Foreground(rcFg)
		lines := strings.Split(m.input.View(), "\n")
		for i, l := range lines {
			p := "  "
			if i == 0 {
				p = "› "
			}
			lines[i] = prompt.Render(p) + l
		}
		return replyLine + strings.Join(lines, "\n")
	}
	hint := ""
	if m.openJID != "" {
		hint = lipgloss.NewStyle().Foreground(rcDim).Render("press i to send, r to reply")
	}
	return replyLine + hint + strings.Repeat("\n", composeRows-1)
}

// renderReactRow draws the "e" emoji-reaction picker as a single line:
// each candidate emoji, with sel picked out in a filled highlight.
// reactGridCols returns how many reactionEmojis cells fit in one row of
// width columns, so renderReactGrid's wrapping and the "up"/"down" picker
// navigation agree on the same row width.
func reactGridCols(width int) int {
	n, w := 0, 0
	for _, e := range reactionEmojis {
		cw := lipgloss.Width(" " + e + " ")
		if n > 0 && w+cw > width {
			break
		}
		w += cw
		n++
	}
	return max(n, 1)
}

// renderReactGrid draws the "e" emoji-reaction picker: a label, the emoji
// grid (wrapped to width, sel picked out in a filled highlight), and a key
// hint, padded/truncated to exactly 1+composeRows lines so opening it
// doesn't shift the message pane's height.
func renderReactGrid(sel, width int) string {
	const totalLines = 1 + composeRows
	cols := reactGridCols(width)
	lines := []string{lipgloss.NewStyle().Foreground(rcYellow).Render("react to message:")}
	for i := 0; i < len(reactionEmojis); i += cols {
		end := min(i+cols, len(reactionEmojis))
		var row strings.Builder
		for j := i; j < end; j++ {
			cell := " " + reactionEmojis[j] + " "
			if j == sel {
				cell = lipgloss.NewStyle().Background(rcYellow).Render(cell)
			}
			row.WriteString(cell)
		}
		lines = append(lines, row.String())
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(rcDim).Render("←/→/↑/↓ pick · enter send · esc cancel"))
	for len(lines) < totalLines {
		lines = append(lines, "")
	}
	return strings.Join(lines[:totalLines], "\n")
}

// renderDeleteConfirm draws the "d" delete-confirmation prompt: a question
// line and a key hint, padded to exactly 1+composeRows lines like
// renderReactGrid. onlyForMe omits the "for everyone" option when the
// target message isn't ours.
func renderDeleteConfirm(onlyForMe bool) string {
	const totalLines = 1 + composeRows
	hint := "1/y: delete for me   esc: cancel"
	if !onlyForMe {
		hint = "1: delete for me   2: delete for everyone   esc: cancel"
	}
	lines := []string{
		lipgloss.NewStyle().Foreground(rcYellow).Render("delete this message?"),
		lipgloss.NewStyle().Foreground(rcDim).Render(hint),
	}
	for len(lines) < totalLines {
		lines = append(lines, "")
	}
	return strings.Join(lines[:totalLines], "\n")
}

// renderMentionRow draws the "@name" autocomplete dropdown as a single
// line: each candidate's name, with sel picked out in a filled highlight.
func renderMentionRow(cands []mentionCandidate, sel int) string {
	label := lipgloss.NewStyle().Foreground(rcYellow).Render("@ mention:")
	if len(cands) == 0 {
		return label + " " + lipgloss.NewStyle().Foreground(rcDim).Render("no match")
	}
	parts := make([]string, len(cands))
	for i, c := range cands {
		style := lipgloss.NewStyle().Foreground(rcFg)
		if i == sel%len(cands) {
			style = lipgloss.NewStyle().Foreground(rcBg).Background(rcYellow).Bold(true)
		}
		parts[i] = style.Render(c.name)
	}
	return label + " " + strings.Join(parts, "  ")
}

// listBottomLine renders the chat list's bottom row: a dim hint pointing at
// the "ctrl+/" chat-search modal (see renderChatSearch), which does its own
// searching full-screen rather than filtering this list in place.
func (m model) listBottomLine() string {
	return lipgloss.NewStyle().Foreground(rcDim).Render(keyHint("browse", "searchChats", "^g") + ": search chats")
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
// imageEscapes is the kitty graphics output the current screen calls for:
// the selected message's inline image on the main view, or just a delete
// of any previous one when a popup or full-screen overlay covers it (so it
// isn't drawn over them). Update emits it via tea.Raw whenever it changes.
func (m model) imageEscapes() string {
	if !kittySupported() {
		return ""
	}
	if !m.ready || m.showReactors || m.showHelp || m.chatSearchOpen || m.err != "" || m.notice != "" {
		return kittyDeleteImage(1)
	}
	return m.inlineImageEscapes()
}

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
	rel := boxLine - m.viewport.YOffset()
	if rel < 0 || rel >= m.viewport.Height() {
		return b.String()
	}
	availRows := imageBoxRows
	if rel+availRows > m.viewport.Height() {
		availRows = m.viewport.Height() - rel
	}
	if availRows < 1 {
		return b.String()
	}
	cols, rows := fitImage(e.w, e.h, m.msgPaneWidth()-2, availRows)
	topRow := 4 + rel        // msgPane border(1) + header(1) + divider(1), then rel lines into the viewport
	leftCol := listWidth + 4 // listPane width+border, then msgPane's own border+inset
	// Save/restore the cursor around the absolute move: bubbletea's
	// renderer repaints relative to where it believes the cursor is (end
	// of the footer), so leaving it parked at the image would shift the
	// next frame's lines onto the wrong rows.
	b.WriteString("\x1b7")
	fmt.Fprintf(&b, "\x1b[%d;%dH", topRow, leftCol)
	b.WriteString(kittyShowImage(e.png, imgID, cols, rows))
	b.WriteString("\x1b8")
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

func borderColor(active bool) color.Color {
	if active {
		return rcGreen
	}
	return rcBorder
}

func renderChatList(chats []Chat, cursor int, openJID string, focused bool, width, height int, bottomLine string, mentionedChats map[string]bool) string {
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
		mentioned := mentionedChats[c.JID]
		name := truncate(c.Name, nameW)
		badge := ""
		if c.UnreadCount > 0 {
			if mentioned {
				badge = fmt.Sprintf(" (@%d)", c.UnreadCount)
			} else {
				badge = fmt.Sprintf(" (%d)", c.UnreadCount)
			}
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
		switch {
		case mentioned:
			style = style.Foreground(rcRed)
		case c.UnreadCount > 0 && c.Muted():
			// A muted chat's unread count stays unaccented (dim) rather
			// than the usual yellow, so it doesn't compete for attention
			// with chats the user hasn't muted.
			style = style.Foreground(rcDim)
		case c.UnreadCount > 0:
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
		// lipgloss v2 counts the border in Width/Height, hence the +2s.
		Width(width + 2).
		Height(height + 2).
		Render(content)
}

// renderMessages builds the viewport content. selectedIdx's message, if
// any, is drawn on a highlighted background across its full width, and is
// the only message whose image (if it has one) appears in the preview
// pane — see imagePaneEscapes. lineStart[i] is the first content line of
// msgs[i] (lineStart[len(msgs)] is the total line count), used to scroll a
// selection fully into view.
// captionAwareText returns a message's body text, preferring MediaCaption
// over wacli's generic "Sent <type>" DisplayText for media messages, while
// keeping any "> quoted\n" reply header DisplayText already built.
func captionAwareText(msg Message) string {
	text := msg.DisplayText
	if msg.MediaType == "" || msg.MediaCaption == "" {
		return text
	}
	if quote, _ := splitQuotedReply(text, msg.Text); quote != "" {
		return "> " + quote + "\n" + msg.MediaCaption
	}
	return msg.MediaCaption
}

// ownText is the part of msg's DisplayText that the sender actually wrote
// (as opposed to wacli's "> quoted" reply header), used to find where a
// multi-line quote ends: the caption for captioned media, else Text, else
// (caption-less media, whose Text is empty) wacli's generic "Sent <type>".
func ownText(msg Message) string {
	switch {
	case msg.MediaType != "" && msg.MediaCaption != "":
		return msg.MediaCaption
	case msg.Text != "":
		return msg.Text
	case msg.MediaType != "":
		return "Sent " + msg.MediaType
	}
	return ""
}

func renderMessages(msgs []Message, width int, selectedIdx int, unread map[string]bool, status map[string]sendLabel, reactors map[string][]reactionDetail) (string, []int) {
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
	// Background is always set explicitly (never left to an outer
	// wrapper): lipgloss renders an empty string's width-padding as a
	// bare, unstyled reset-then-spaces, so a row with no text (the
	// blank line between messages) would otherwise show no background
	// at all across its full width.
	row := func(s string, fg color.Color, bold bool, bg color.Color) string {
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
		if msg.FromMe {
			rowBg = rcMeBg
		}
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
			writeLine(row(marker, rcImage, true, rowBg))
			if selected {
				// Reserve blank rows for the inline preview drawn by
				// inlineImageEscapes, so the text below doesn't overlap it.
				for range imageBoxRows {
					writeLine(row("", rcFg, false, rowBg))
				}
			}
		}

		text := captionAwareText(msg)
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
			quote, body := splitQuotedReply(text, ownText(msg))
			if quote != "" {
				for _, l := range wrapPlain(collapseBlankLines(quote), width-2) {
					writeLine(row("│ "+l, rcQuote, false, rowBg))
				}
			}
			for _, l := range wrapPlain(collapseBlankLines(body), width) {
				writeLine(row(l, textColor, false, rowBg))
			}
		}
		if entries := reactors[msg.MsgID]; len(entries) > 0 {
			// Small rounded-looking chips under the message's left edge,
			// one per emoji, the way WhatsApp shows reactions under a
			// bubble — not one flat bar, and not the same color as the
			// row/chat selection highlights.
			chipStyle := lipgloss.NewStyle().Background(rcReact).Foreground(rcFg)
			gapStyle := lipgloss.NewStyle().Background(rowBg)
			var chips strings.Builder
			chips.WriteString(gapStyle.Render(" "))
			for i, t := range groupReactions(entries) {
				if i > 0 {
					chips.WriteString(gapStyle.Render(" "))
				}
				chips.WriteString(chipStyle.Render(fmt.Sprintf(" %s %d ", t.emoji, len(t.names))))
			}
			pad := max(width-lipgloss.Width(chips.String()), 0)
			chips.WriteString(gapStyle.Render(strings.Repeat(" ", pad)))
			writeLine(chips.String())
		}
		writeLine(row("", rcFg, false, rowBg))
	}
	lineStart = append(lineStart, line)
	return b.String(), lineStart
}

// splitQuotedReply splits a reply's DisplayText into the quoted original
// (wacli's "> quoted\nbody" header, see wacli's buildDisplayText) and the
// actual reply body, so renderMessages can style the two differently
// instead of showing them as one indistinguishable block. quote == "" means
// text isn't a reply.
//
// wacli prefixes only the quote's first line with "> ", so a multi-line
// quote can't be delimited by the first newline. When own (the reply's
// own text, see ownText) is what DisplayText ends with, everything between
// the "> " and it is the quote; otherwise fall back to the first newline.
func splitQuotedReply(text, own string) (quote, body string) {
	if strings.HasPrefix(text, "> ") {
		rest := text[2:]
		if own != "" && strings.HasSuffix(rest, "\n"+own) {
			return strings.TrimRight(rest[:len(rest)-len(own)-1], "\n"), own
		}
		if quote, body, ok := strings.Cut(rest, "\n"); ok {
			return quote, body
		}
		return rest, ""
	}
	return "", text
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
	chatJID := flag.String("chat", "", "open directly into this chat (JID), e.g. from a dock/menu-bar widget")
	initKeys := flag.Bool("init-keys", false, "write every keybinding (defaults plus your current overrides) to the keys.toml config as an editable template, backing up any existing file, then exit")
	printKeys := flag.Bool("print-keys", false, "print every keybinding in keys.toml format to stdout, then exit")
	flag.Parse()

	if f := initLogging(); f != nil {
		defer f.Close()
	}
	appKeys = loadKeymap()
	if *printKeys {
		fmt.Print(keymapTOML(appKeys))
		return
	}
	if *initKeys {
		path, bak, err := initKeymapFile(appKeys)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if bak != "" {
			fmt.Println("backed up", bak)
		}
		fmt.Println("wrote", path)
		return
	}

	m := initialModel()
	m.startJID = *chatJID
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
