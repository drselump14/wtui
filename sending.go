package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Outgoing messages are sent in the background: pressing enter appends a
// local placeholder to the chat right away and queues the actual wacli
// send. Sends run one at a time, in order, so concurrent wacli processes
// never fight over its store and messages arrive in the order typed.

const pendingIDPrefix = "pending:"

type sendState int

const (
	sendQueued sendState = iota
	sendInFlight
	sendDone   // wacli confirmed; dropped once a reload shows the real message
	sendFailed // kept, with its error, so the text isn't lost
)

type pendingSend struct {
	id         string // local MsgID, pendingIDPrefix + counter
	jid        string
	text       string // message text, or a media send's caption ("" if none; always "" for a sticker)
	mediaPath  string // non-"" for a media send: local file path given to `wacli send file` (or `wacli send sticker`)
	mediaOwned bool   // true if mediaPath is a wtui temp file to delete once wacli has read it; false for a user file (ctrl+t attach), left untouched on disk
	sticker    bool   // true to send mediaPath via `wacli send sticker` instead of `wacli send file`
	replyTo    string
	mentions   []string // phone numbers to tag via --mention (text sends only; wacli's file send has no --mention flag)
	at         time.Time
	state      sendState
	err        string
}

func (p pendingSend) isMedia() bool { return p.mediaPath != "" }

type pendingSentMsg struct {
	id  string
	err error
}

type sendTickMsg struct{}

// sendTick drives the spinner on in-flight placeholders.
func sendTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return sendTickMsg{} })
}

func isPendingID(id string) bool { return strings.HasPrefix(id, pendingIDPrefix) }

// queueSend appends a placeholder for text and starts sending it if no
// other send is in flight. The placeholder becomes the selected message.
func (m *model) queueSend(jid, text, replyTo string, mentions []string) tea.Cmd {
	m.pendingSeq++
	m.pending = append(m.pending, pendingSend{
		id:       fmt.Sprintf("%s%d", pendingIDPrefix, m.pendingSeq),
		jid:      jid,
		text:     text,
		replyTo:  replyTo,
		mentions: mentions,
		at:       time.Now(),
	})
	if jid == m.openJID && m.openMsgs != nil {
		m.mergePending()
		m.selectedMsg = len(m.openMsgs) - 1
		m.rebuildContent()
		m.scrollToSelection()
	}
	return m.startNextSend()
}

// queueMediaSend appends a placeholder for a file (pasted from the
// clipboard, or attached from disk via ctrl+t) at path, with an optional
// caption, and starts sending it if no other send is in flight. owned
// marks path as a wtui temp file to delete once sent; a user's own
// attached file is left on disk either way.
func (m *model) queueMediaSend(jid, path, caption, replyTo string, owned bool) tea.Cmd {
	m.pendingSeq++
	id := fmt.Sprintf("%s%d", pendingIDPrefix, m.pendingSeq)
	m.pending = append(m.pending, pendingSend{
		id:         id,
		jid:        jid,
		text:       caption,
		mediaPath:  path,
		mediaOwned: owned,
		replyTo:    replyTo,
		at:         time.Now(),
	})
	// The file is already on disk, so the preview can be built straight
	// from it instead of round-tripping through wacli's media download
	// (which only knows about messages that exist on the server).
	if png, w, h, err := loadImagePNG(path); err == nil {
		m.imgCache[id] = &imgEntry{png: png, w: w, h: h}
	}
	if jid == m.openJID && m.openMsgs != nil {
		m.mergePending()
		m.selectedMsg = len(m.openMsgs) - 1
		m.rebuildContent()
		m.scrollToSelection()
	}
	return m.startNextSend()
}

// queueStickerSend appends a placeholder for a sticker (a WebP image
// attached from disk via ctrl+t) at path, and starts sending it if no
// other send is in flight. Stickers carry no caption, unlike a plain file
// send. owned marks path as a wtui temp file to delete once sent; a user's
// own attached file is left on disk either way.
func (m *model) queueStickerSend(jid, path, replyTo string, owned bool) tea.Cmd {
	m.pendingSeq++
	id := fmt.Sprintf("%s%d", pendingIDPrefix, m.pendingSeq)
	m.pending = append(m.pending, pendingSend{
		id:         id,
		jid:        jid,
		mediaPath:  path,
		mediaOwned: owned,
		sticker:    true,
		replyTo:    replyTo,
		at:         time.Now(),
	})
	if png, w, h, err := loadImagePNG(path); err == nil {
		m.imgCache[id] = &imgEntry{png: png, w: w, h: h}
	}
	if jid == m.openJID && m.openMsgs != nil {
		m.mergePending()
		m.selectedMsg = len(m.openMsgs) - 1
		m.rebuildContent()
		m.scrollToSelection()
	}
	return m.startNextSend()
}

// startNextSend launches the oldest queued send unless one is in flight.
func (m *model) startNextSend() tea.Cmd {
	for i := range m.pending {
		if m.pending[i].state == sendInFlight {
			return nil
		}
	}
	for i := range m.pending {
		p := &m.pending[i]
		if p.state != sendQueued {
			continue
		}
		p.state = sendInFlight
		id, jid, text, mediaPath, mediaOwned, sticker, replyTo, mentions := p.id, p.jid, p.text, p.mediaPath, p.mediaOwned, p.sticker, p.replyTo, p.mentions
		send := func() tea.Msg {
			if mediaPath != "" {
				var err error
				if sticker {
					err = sendSticker(jid, mediaPath, replyTo)
				} else {
					err = sendFile(jid, mediaPath, text, replyTo)
				}
				if err == nil && mediaOwned {
					os.Remove(mediaPath) // wacli has read it; no longer needed
				}
				return pendingSentMsg{id: id, err: err}
			}
			return pendingSentMsg{id: id, err: sendText(jid, text, replyTo, mentions)}
		}
		if m.sendTicking {
			return send
		}
		m.sendTicking = true
		return tea.Batch(send, sendTick())
	}
	return nil
}

// finishSend records a send's outcome, reloads its chat on success, and
// starts the next queued send.
func (m *model) finishSend(msg pendingSentMsg) tea.Cmd {
	var jid string
	for i := range m.pending {
		p := &m.pending[i]
		if p.id != msg.id {
			continue
		}
		jid = p.jid
		if msg.err != nil {
			p.state, p.err = sendFailed, msg.err.Error()
		} else {
			p.state = sendDone
		}
	}
	cmds := []tea.Cmd{m.startNextSend()}
	if jid == m.openJID {
		m.mergePending()
		m.rebuildContent()
		if msg.err == nil {
			cmds = append(cmds, loadMessages(jid, m.mentionNames))
		}
	}
	return tea.Batch(cmds...)
}

// dropDoneSends forgets confirmed sends for jid whose real message shows
// up in loaded (a FromMe message with the same text, not already claimed
// by an earlier placeholder). A reload that raced the send and doesn't
// include it yet leaves the "✓ sent" placeholder in place.
func (m *model) dropDoneSends(jid string, loaded []Message) {
	claimed := map[int]bool{}
	found := func(p pendingSend) bool {
		// loaded's DisplayText has already had its @mentions expanded from
		// raw digits to names (loadMessages), but p.text is still exactly
		// what the user typed; expand it the same way before comparing, or
		// a resolved mention makes this never match and the placeholder
		// (still showing raw digits) lingers as a duplicate forever.
		text := expandMentions(p.text, m.mentionNames)
		for i := len(loaded) - 1; i >= 0; i-- {
			msg := loaded[i]
			// For replies wacli prefixes DisplayText with the quoted
			// text ("> quoted\nreply"), so match on the trailing part.
			sameText := msg.DisplayText == text || strings.HasSuffix(msg.DisplayText, "\n"+text)
			sameKind := p.isMedia() == (msg.MediaType != "")
			if claimed[i] || !msg.FromMe || !sameText || !sameKind {
				continue
			}
			if ts, err := time.Parse(time.RFC3339, msg.Timestamp); err == nil && ts.Before(p.at.Add(-time.Minute)) {
				continue // an older message that happens to have the same text
			}
			claimed[i] = true
			return true
		}
		return false
	}
	kept := m.pending[:0]
	for _, p := range m.pending {
		if p.jid == jid && p.state == sendDone && found(p) {
			continue
		}
		kept = append(kept, p)
	}
	m.pending = kept
}

// dropFailedSend removes a failed placeholder (by local MsgID).
func (m *model) dropFailedSend(id string) bool {
	for i, p := range m.pending {
		if p.id == id && p.state == sendFailed {
			if p.isMedia() && p.mediaOwned {
				os.Remove(p.mediaPath)
			}
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			return true
		}
	}
	return false
}

// retrySend requeues a failed placeholder.
func (m *model) retrySend(id string) tea.Cmd {
	for i := range m.pending {
		if p := &m.pending[i]; p.id == id && p.state == sendFailed {
			p.state, p.err = sendQueued, ""
			return m.startNextSend()
		}
	}
	return nil
}

func (m model) sendsActive() bool {
	for _, p := range m.pending {
		if p.state == sendQueued || p.state == sendInFlight {
			return true
		}
	}
	return len(m.reactSends) > 0 || len(m.delSends) > 0
}

// mergePending rebuilds openMsgs as the loaded server messages followed
// by placeholders for the open chat's pending sends.
func (m *model) mergePending() {
	if m.serverMsgs == nil {
		return // chat not loaded yet; placeholders appear with the first load
	}
	msgs := append([]Message(nil), m.serverMsgs...)
	for _, p := range m.pending {
		if p.jid != m.openJID {
			continue
		}
		msg := Message{
			ChatJID:     p.jid,
			MsgID:       p.id,
			Timestamp:   p.at.Format(time.RFC3339),
			FromMe:      true,
			DisplayText: p.text,
			QuotedMsgID: p.replyTo,
		}
		switch {
		case p.sticker:
			msg.MediaType = "sticker"
		case p.isMedia():
			msg.MediaType = "image"
		}
		msgs = append(msgs, msg)
	}
	m.openMsgs = msgs
	if m.selectedMsg >= len(m.openMsgs) {
		m.selectedMsg = len(m.openMsgs) - 1
	}
}

// sendStatusLabels maps placeholder MsgIDs to the status shown after
// their header ("⠋", "✗ failed: …").
func (m model) sendStatusLabels() map[string]sendLabel {
	if len(m.pending) == 0 {
		return nil
	}
	out := map[string]sendLabel{}
	for _, p := range m.pending {
		switch p.state {
		case sendQueued:
			out[p.id] = sendLabel{text: "◷ queued"}
		case sendInFlight:
			out[p.id] = sendLabel{text: spinnerFrames[m.sendFrame]}
		case sendDone:
			out[p.id] = sendLabel{text: "✓ sent"}
		case sendFailed:
			out[p.id] = sendLabel{text: "✗ failed: " + p.err + "  (R: retry, x: discard)", failed: true}
		}
	}
	return out
}

type sendLabel struct {
	text   string
	failed bool
}

// bgTaskSummary is the background-task section's one-line progress
// summary of in-flight message/file sends and reactions, or "" when
// nothing is in flight.
func (m model) bgTaskSummary() string {
	var msgs, files, stickers int
	for _, p := range m.pending {
		if p.state != sendQueued && p.state != sendInFlight {
			continue
		}
		switch {
		case p.sticker:
			stickers++
		case p.isMedia():
			files++
		default:
			msgs++
		}
	}
	reacts := len(m.reactSends)
	dels := len(m.delSends)
	if msgs == 0 && files == 0 && stickers == 0 && reacts == 0 && dels == 0 {
		return ""
	}
	var sending []string
	if msgs > 0 {
		sending = append(sending, pluralize(msgs, "message"))
	}
	if reacts > 0 {
		sending = append(sending, pluralize(reacts, "reaction"))
	}
	if files > 0 {
		sending = append(sending, pluralize(files, "file"))
	}
	if stickers > 0 {
		sending = append(sending, pluralize(stickers, "sticker"))
	}
	var clauses []string
	if len(sending) > 0 {
		clauses = append(clauses, "sending "+strings.Join(sending, ", "))
	}
	if dels > 0 {
		clauses = append(clauses, "deleting "+pluralize(dels, "message"))
	}
	return spinnerFrames[m.sendFrame] + " " + strings.Join(clauses, "; ") + "…"
}

func pluralize(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
