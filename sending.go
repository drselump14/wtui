package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	id      string // local MsgID, pendingIDPrefix + counter
	jid     string
	text    string
	replyTo string
	at      time.Time
	state   sendState
	err     string
}

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
func (m *model) queueSend(jid, text, replyTo string) tea.Cmd {
	m.pendingSeq++
	m.pending = append(m.pending, pendingSend{
		id:      fmt.Sprintf("%s%d", pendingIDPrefix, m.pendingSeq),
		jid:     jid,
		text:    text,
		replyTo: replyTo,
		at:      time.Now(),
	})
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
		id, jid, text, replyTo := p.id, p.jid, p.text, p.replyTo
		send := func() tea.Msg {
			return pendingSentMsg{id: id, err: sendText(jid, text, replyTo)}
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
			cmds = append(cmds, loadMessages(jid))
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
		for i := len(loaded) - 1; i >= 0; i-- {
			msg := loaded[i]
			// For replies wacli prefixes DisplayText with the quoted
			// text ("> quoted\nreply"), so match on the trailing part.
			sameText := msg.DisplayText == p.text || strings.HasSuffix(msg.DisplayText, "\n"+p.text)
			if claimed[i] || !msg.FromMe || !sameText {
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
	return false
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
		msgs = append(msgs, Message{
			ChatJID:     p.jid,
			MsgID:       p.id,
			Timestamp:   p.at.Format(time.RFC3339),
			FromMe:      true,
			DisplayText: p.text,
			QuotedMsgID: p.replyTo,
		})
	}
	m.openMsgs = msgs
	if m.selectedMsg >= len(m.openMsgs) {
		m.selectedMsg = len(m.openMsgs) - 1
	}
}

// sendStatusLabels maps placeholder MsgIDs to the status shown after
// their header ("⠋ sending…", "✗ failed: …").
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
			out[p.id] = sendLabel{text: spinnerFrames[m.sendFrame] + " sending…"}
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
